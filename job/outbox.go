package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Publish prepares a job and writes it into the caller's existing PostgreSQL
// transaction. Commit business changes and this outbox row together. IdempotencyKey
// is mandatory so an uncertain publisher commit can be retried without a new effect.
// The caller owns commit/rollback. Its transaction must use the Store's database.
func (s *Store) Publish(ctx context.Context, tx *sql.Tx, request Request) (string, error) {
	if tx == nil || request.IdempotencyKey == "" {
		return "", errors.New("job: outbox requires a transaction and idempotency key")
	}
	envelope, err := s.prepare(ctx, request)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("job: encode outbox: %w", err)
	}
	id, err := newID()
	if err != nil {
		return "", err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO trest_job_outbox(id,envelope) VALUES($1,$2::json)`, id, string(data))
	if err != nil {
		return "", fmt.Errorf("job: publish outbox: %w", err)
	}
	return id, nil
}

// DispatchOne atomically transfers an outbox record to the queue and records the
// resulting job ID. Concurrent dispatchers use SKIP LOCKED. Duplicate deliveries
// resolve through queue idempotency. A permanently conflicting row is marked failed
// so it does not block later rows. No remote systems are called in this transaction.
func (s *Store) DispatchOne(ctx context.Context) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("job: begin dispatch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT id,envelope FROM trest_job_outbox WHERE state='pending' ORDER BY created_at,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoJob
	}
	if err != nil {
		return "", fmt.Errorf("job: read outbox: %w", err)
	}
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Version != EnvelopeVersion {
		return "", errors.New("job: unsupported or corrupt outbox envelope; operator intervention required")
	}
	queued, _, insertErr := s.insert(ctx, tx, envelope)
	if insertErr != nil && !errors.Is(insertErr, ErrConflict) {
		return "", insertErr
	}
	state, code := "dispatched", ""
	if insertErr != nil {
		state, code = "failed", "idempotency_conflict"
	}
	_, err = tx.ExecContext(ctx, `UPDATE trest_job_outbox SET state=$2,job_id=$3,error_code=$4,dispatched_at=clock_timestamp() WHERE id=$1`, id, state, nullableString(queued.ID), code)
	if err != nil {
		return "", fmt.Errorf("job: mark dispatch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("job: commit dispatch (outcome may be unknown): %w", err)
	}
	return queued.ID, insertErr
}

// OutboxStatus is safe for administrative diagnostics; it contains no business input.
type OutboxStatus struct{ ID, State, JobID, ErrorCode string }

func (s *Store) Outbox(ctx context.Context, id string) (OutboxStatus, error) {
	var status OutboxStatus
	var jobID sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,state,job_id,error_code FROM trest_job_outbox WHERE id=$1`, id).Scan(&status.ID, &status.State, &jobID, &status.ErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxStatus{}, ErrNotFound
	}
	if err != nil {
		return OutboxStatus{}, fmt.Errorf("job: outbox status: %w", err)
	}
	status.JobID = jobID.String
	return status, nil
}

// CleanupOutbox removes dispatched records only after the configured retention.
// Failed rows remain visible for operator inspection.
func (s *Store) CleanupOutbox(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("job: cleanup limit must be between 1 and 1000")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM trest_job_outbox WHERE id IN
	(SELECT id FROM trest_job_outbox WHERE state='dispatched' AND dispatched_at<clock_timestamp()-($1*interval '1 second')
	ORDER BY dispatched_at,id FOR UPDATE SKIP LOCKED LIMIT $2)`, durationSeconds(s.options.Retention), limit)
	if err != nil {
		return 0, fmt.Errorf("job: outbox cleanup: %w", err)
	}
	return result.RowsAffected()
}
