package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func validateOperator(actor, reason string) error {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" || len(actor) > 512 || len(reason) > 1024 || strings.ContainsRune(actor+reason, '\x00') {
		return errors.New("job: bounded operator identity and reason are required")
	}
	return nil
}

// Cancel cancels pending work immediately, or requests cooperative cancellation of
// a running attempt. The caller must authorize the operator. Running cancellation
// cannot undo committed effects; its eventual outcome may be uncertain.
func (s *Store) Cancel(ctx context.Context, id, actor, reason string) error {
	if err := validateOperator(actor, reason); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("job: begin cancellation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM trest_job_queue WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if job.State != Pending && job.State != Running {
		return ErrInvalidState
	}
	_, err = tx.ExecContext(ctx, `UPDATE trest_job_queue SET cancel_requested=true,
	state=CASE WHEN state='pending' THEN 'cancelled' ELSE state END,
	finished_at=CASE WHEN state='pending' THEN clock_timestamp() ELSE finished_at END,
	updated_at=clock_timestamp(),dedupe_until=GREATEST(dedupe_until,clock_timestamp()+($2*interval '1 second')) WHERE id=$1`, id, durationSeconds(s.options.IdempotencyRetention))
	if err != nil {
		return fmt.Errorf("job: cancel: %w", err)
	}
	if err := event(ctx, tx, id, job.Attempts, "cancellation_requested", actor, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryRequest records a deliberate operator decision. Retrying uncertain work
// can repeat an external effect. Reconcile it before calling Retry.
type RetryRequest struct {
	Actor              string
	Reason             string
	AdditionalAttempts int
	Deadline           time.Time
}

// Retry preserves job identity, input, principal, and history. It adds a bounded
// attempt allowance and reauthorizes when a worker executes the next attempt.
func (s *Store) Retry(ctx context.Context, id string, request RetryRequest) error {
	if err := validateOperator(request.Actor, request.Reason); err != nil {
		return err
	}
	if request.AdditionalAttempts < 1 || request.AdditionalAttempts > 100 {
		return errors.New("job: additional attempts must be between 1 and 100")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("job: begin retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM trest_job_queue WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if job.State != Failed && job.State != Uncertain && job.State != Cancelled {
		return ErrInvalidState
	}
	deadline := job.Envelope.Request.Deadline
	if !request.Deadline.IsZero() {
		deadline = request.Deadline
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if !deadline.IsZero() && !deadline.After(now) {
		return errors.New("job: provide a new future deadline before retry")
	}
	_, err = tx.ExecContext(ctx, `UPDATE trest_job_queue SET state='pending',max_attempts=attempts+$2,available_at=clock_timestamp(),
	deadline_at=$3,finished_at=NULL,cancel_requested=false,output=NULL,error_code='',attempt_token=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1`, id, request.AdditionalAttempts, nullableTime(deadline))
	if err != nil {
		return fmt.Errorf("job: retry: %w", err)
	}
	if err := event(ctx, tx, id, job.Attempts, "manual_retry", request.Actor, request.Reason); err != nil {
		return err
	}
	return tx.Commit()
}

// Resolve records evidence gathered outside the queue about an uncertain attempt;
// it does not execute the handler. Only a reconciled successful result may include Output.
func (s *Store) Resolve(ctx context.Context, id, actor, reason string, outcome Outcome) error {
	if err := validateOperator(actor, reason); err != nil {
		return err
	}
	if outcome.State != Succeeded && outcome.State != Failed && outcome.State != Cancelled {
		return ErrInvalidState
	}
	if outcome.State == Succeeded && (len(outcome.Output) > s.options.MaxOutputBytes || !json.Valid(outcome.Output)) {
		return errors.New("job: reconciled output exceeds bounds or is invalid json")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM trest_job_queue WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if job.State != Uncertain {
		return ErrInvalidState
	}
	var output any
	if outcome.State == Succeeded {
		output = string(outcome.Output)
	}
	_, err = tx.ExecContext(ctx, `UPDATE trest_job_queue SET state=$2,output=$3::json,error_code='reconciled',finished_at=clock_timestamp(),updated_at=clock_timestamp(),
	dedupe_until=GREATEST(dedupe_until,clock_timestamp()+($4*interval '1 second')) WHERE id=$1`, id, outcome.State, output, durationSeconds(s.options.IdempotencyRetention))
	if err != nil {
		return fmt.Errorf("job: resolve: %w", err)
	}
	if err := event(ctx, tx, id, job.Attempts, "reconciled", actor, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// Events returns a bounded redacted history in sequence order. afterSequence is a
// continuation cursor. Access control belongs to the administrative caller.
func (s *Store) Events(ctx context.Context, id string, afterSequence int64, limit int) ([]Event, error) {
	if limit < 1 || limit > 1000 || afterSequence < 0 {
		return nil, errors.New("job: invalid history bounds")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,job_id,attempt,kind,actor,detail,created_at FROM trest_job_event
	WHERE job_id=$1 AND sequence>$2 ORDER BY sequence LIMIT $3`, id, afterSequence, limit)
	if err != nil {
		return nil, fmt.Errorf("job: history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Sequence, &e.JobID, &e.Attempt, &e.Kind, &e.Actor, &e.Detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// List returns bounded administrative queue records. Empty state lists all states.
func (s *Store) List(ctx context.Context, state State, limit int) ([]Job, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("job: list limit must be between 1 and 1000")
	}
	switch state {
	case "", Pending, Running, Succeeded, Failed, Cancelled, Uncertain:
	default:
		return nil, ErrInvalidState
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM trest_job_queue WHERE ($1='' OR state=$1) ORDER BY created_at,id LIMIT $2`, state, limit)
	if err != nil {
		return nil, fmt.Errorf("job: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// Cleanup deletes only completed, reconciled records older than both retention
// windows, including their event history. Uncertain jobs are retained until an
// operator resolves or retries them. Once deleted, an idempotency key can be reused.
func (s *Store) Cleanup(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("job: cleanup limit must be between 1 and 1000")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM trest_job_queue WHERE id IN
	(SELECT id FROM trest_job_queue WHERE state IN ('succeeded','failed','cancelled')
	AND finished_at<clock_timestamp()-($1*interval '1 second') AND dedupe_until<clock_timestamp()
	ORDER BY finished_at,id FOR UPDATE SKIP LOCKED LIMIT $2)`, durationSeconds(s.options.Retention), limit)
	if err != nil {
		return 0, fmt.Errorf("job: cleanup: %w", err)
	}
	return result.RowsAffected()
}
