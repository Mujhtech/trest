package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const jobColumns = `id,envelope_version,operation_id,payload_version,input,input_hash,principal_id,tenant_id,caller_scope,
idempotency_key,correlation_id,state,attempts,max_attempts,retry_safe,available_at,deadline_at,lease_expires_at,
attempt_token,cancel_requested,output,error_code,created_at,updated_at,finished_at,trace_link`

type scanner interface{ Scan(...any) error }

func scanJob(row scanner) (Job, error) {
	var job Job
	var deadline, lease, finished sql.NullTime
	var token, key sql.NullString
	var trace, input, output []byte
	err := row.Scan(&job.ID, &job.Envelope.Version, &job.Envelope.Request.OperationID, &job.Envelope.Request.PayloadVersion,
		&input, &job.Envelope.InputHash, &job.Envelope.Principal.ID, &job.Envelope.Principal.TenantID,
		&job.Envelope.Request.CallerScope, &key, &job.Envelope.Request.CorrelationID, &job.State, &job.Attempts,
		&job.Envelope.Request.MaxAttempts, &job.Envelope.Request.RetrySafe, &job.AvailableAt, &deadline, &lease, &token,
		&job.CancelRequested, &output, &job.ErrorCode, &job.CreatedAt, &job.UpdatedAt, &finished, &trace)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNotFound
		}
		return Job{}, fmt.Errorf("job: read record: %w", err)
	}
	if err := json.Unmarshal(trace, &job.Envelope.Trace); err != nil {
		return Job{}, fmt.Errorf("job: invalid trace link: %w", err)
	}
	job.Envelope.Request.Input = input
	job.Output = output
	job.Envelope.Request.IdempotencyKey = key.String
	job.Envelope.Request.NotBefore = job.AvailableAt
	job.Envelope.Request.Deadline = deadline.Time
	job.LeaseExpiresAt = lease.Time
	job.FinishedAt = finished.Time
	job.AttemptToken = token.String
	return job, nil
}

// Enqueue authorizes and persists an operation. A duplicate idempotency key returns
// the existing job and created=false; different input or actor returns ErrConflict.
// Queue deduplication alone does not make handler effects idempotent.
func (s *Store) Enqueue(ctx context.Context, request Request) (job Job, created bool, err error) {
	envelope, err := s.prepare(ctx, request)
	if err != nil {
		return Job{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, false, fmt.Errorf("job: begin enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	job, created, err = s.insert(ctx, tx, envelope)
	if err != nil {
		return Job{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, false, fmt.Errorf("job: commit enqueue (outcome may be unknown): %w", err)
	}
	return job, created, nil
}

func (s *Store) insert(ctx context.Context, tx *sql.Tx, envelope Envelope) (Job, bool, error) {
	id, err := newID()
	if err != nil {
		return Job{}, false, err
	}
	r := envelope.Request
	trace, err := json.Marshal(envelope.Trace)
	if err != nil {
		return Job{}, false, err
	}
	var inserted string
	err = tx.QueryRowContext(ctx, `INSERT INTO trest_job_queue
	(id,envelope_version,operation_id,payload_version,input,input_hash,principal_id,tenant_id,caller_scope,idempotency_key,
	correlation_id,state,max_attempts,retry_safe,available_at,deadline_at,dedupe_until,trace_link)
	VALUES ($1,$2,$3,$4,$5::json,$6,$7,$8,$9,$10,$11,'pending',$12,$13,COALESCE($14::timestamptz,clock_timestamp()),$15,
	clock_timestamp()+($16*interval '1 second'),$17::json) ON CONFLICT DO NOTHING RETURNING id`,
		id, envelope.Version, r.OperationID, r.PayloadVersion, string(r.Input), envelope.InputHash, envelope.Principal.ID,
		envelope.Principal.TenantID, r.CallerScope, nullableString(r.IdempotencyKey), r.CorrelationID, r.MaxAttempts,
		r.RetrySafe, nullableTime(r.NotBefore), nullableTime(r.Deadline), durationSeconds(s.options.IdempotencyRetention), string(trace)).Scan(&inserted)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return Job{}, false, fmt.Errorf("job: insert: %w", err)
		}
		if r.IdempotencyKey == "" {
			return Job{}, false, errors.New("job: generated id collision")
		}
		existing, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM trest_job_queue
		WHERE tenant_id=$1 AND operation_id=$2 AND caller_scope=$3 AND idempotency_key=$4`,
			envelope.Principal.TenantID, r.OperationID, r.CallerScope, r.IdempotencyKey))
		if err != nil {
			return Job{}, false, err
		}
		if existing.Envelope.InputHash != envelope.InputHash || existing.Envelope.Principal.ID != envelope.Principal.ID {
			return Job{}, false, ErrConflict
		}
		return existing, false, nil
	}
	if err := event(ctx, tx, id, 0, "enqueued", envelope.Principal.ID, ""); err != nil {
		return Job{}, false, err
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM trest_job_queue WHERE id=$1`, id))
	return job, true, err
}

// Get reads an administrative record; applications must authorize access to this API.
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM trest_job_queue WHERE id=$1`, id))
}

// Claim atomically takes one due job. Expired attempts are first marked uncertain;
// they are never silently replayed. Clock decisions use the database clock.
func (s *Store) Claim(ctx context.Context) (Job, error) {
	if _, err := s.Recover(ctx, 100); err != nil {
		return Job{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, fmt.Errorf("job: begin claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM trest_job_queue WHERE state='pending' AND available_at<=clock_timestamp()
	AND attempts<max_attempts AND (deadline_at IS NULL OR deadline_at>clock_timestamp()) ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNoJob
	}
	if err != nil {
		return Job{}, fmt.Errorf("job: select claim: %w", err)
	}
	token, err := newID()
	if err != nil {
		return Job{}, err
	}
	job, err := scanJob(tx.QueryRowContext(ctx, `UPDATE trest_job_queue SET state='running',attempts=attempts+1,
	attempt_token=$2,lease_expires_at=clock_timestamp()+($3*interval '1 second'),updated_at=clock_timestamp()
	WHERE id=$1 RETURNING `+jobColumns, id, token, durationSeconds(s.options.Lease)))
	if err != nil {
		return Job{}, err
	}
	if err := event(ctx, tx, id, job.Attempts, "claimed", "", ""); err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("job: commit claim: %w", err)
	}
	return job, nil
}

// Heartbeat renews only a live attempt and reports cancellation requests. Stale
// attempts cannot renew an expired lease. Callers must stop work when it fails.
func (s *Store) Heartbeat(ctx context.Context, id, token string) (bool, error) {
	var cancel bool
	err := s.db.QueryRowContext(ctx, `UPDATE trest_job_queue SET lease_expires_at=clock_timestamp()+($3*interval '1 second'),updated_at=clock_timestamp()
	WHERE id=$1 AND state='running' AND attempt_token=$2 AND lease_expires_at>clock_timestamp() RETURNING cancel_requested`, id, token, durationSeconds(s.options.Lease)).Scan(&cancel)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrStaleAttempt
	}
	if err != nil {
		return false, fmt.Errorf("job: heartbeat: %w", err)
	}
	return cancel, nil
}

// Outcome describes a known completion, terminal failure, uncertain effect, or
// safe scheduled retry. Pending is accepted only for RetrySafe jobs.
type Outcome struct {
	State      State
	Output     json.RawMessage
	Code       string
	RetryAfter time.Duration
	// NoHandlerExecuted permits retry after preflight errors without RetrySafe.
	// Set this only when execution has not entered application business code.
	NoHandlerExecuted bool
}

// Complete commits only the current unexpired attempt. Completion errors are not
// proof that business effects failed. If this write is lost, recovery is uncertain.
func (s *Store) Complete(ctx context.Context, id, token string, outcome Outcome) error {
	if outcome.State != Succeeded && outcome.State != Failed && outcome.State != Uncertain && outcome.State != Pending {
		return ErrInvalidState
	}
	if len(outcome.Code) > 128 || strings.ContainsRune(outcome.Code, '\x00') {
		return errors.New("job: outcome code must be bounded")
	}
	if outcome.State == Succeeded && (len(outcome.Output) > s.options.MaxOutputBytes || !json.Valid(outcome.Output)) {
		return errors.New("job: output exceeds bound or is invalid json")
	}
	if outcome.RetryAfter < 0 || outcome.RetryAfter > s.options.MaxBackoff {
		return errors.New("job: retry delay outside configured bounds")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("job: begin completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM trest_job_queue
	WHERE id=$1 AND state='running' AND attempt_token=$2 AND lease_expires_at>clock_timestamp() FOR UPDATE`, id, token))
	if errors.Is(err, ErrNotFound) {
		return ErrStaleAttempt
	}
	if err != nil {
		return err
	}
	if outcome.State == Pending {
		if !job.Envelope.Request.RetrySafe && !outcome.NoHandlerExecuted {
			return ErrInvalidState
		}
		if job.CancelRequested {
			outcome.State = Uncertain
			outcome.Code = "cancelled_during_attempt"
		} else if job.Attempts >= job.Envelope.Request.MaxAttempts {
			outcome.State = Failed
			outcome.Code = "retry_exhausted"
		}
	}
	var output any
	if outcome.State == Succeeded {
		output = string(outcome.Output)
	}
	_, err = tx.ExecContext(ctx, `UPDATE trest_job_queue SET state=$3,output=$4::json,error_code=$5,attempt_token=NULL,lease_expires_at=NULL,
	available_at=CASE WHEN $3='pending' THEN clock_timestamp()+($6*interval '1 second') ELSE available_at END,
	finished_at=CASE WHEN $3='pending' THEN NULL ELSE clock_timestamp() END,updated_at=clock_timestamp(),
	dedupe_until=GREATEST(dedupe_until,clock_timestamp()+($7*interval '1 second')) WHERE id=$1 AND attempt_token=$2`,
		id, token, outcome.State, output, outcome.Code, durationSeconds(outcome.RetryAfter), durationSeconds(s.options.IdempotencyRetention))
	if err != nil {
		return fmt.Errorf("job: complete: %w", err)
	}
	if err := event(ctx, tx, id, job.Attempts, string(outcome.State), "", outcome.Code); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("job: commit completion (outcome may be unknown): %w", err)
	}
	return nil
}

// Recover marks bounded expired leases uncertain and expired pending jobs failed.
// It never assumes that a crashed worker caused no external effects.
func (s *Store) Recover(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("job: recovery limit must be between 1 and 1000")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("job: begin recovery: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT id,attempts,state FROM trest_job_queue WHERE
	(state='running' AND lease_expires_at<=clock_timestamp()) OR (state='pending' AND deadline_at<=clock_timestamp())
	ORDER BY updated_at,id FOR UPDATE SKIP LOCKED LIMIT $1`, limit)
	if err != nil {
		return 0, fmt.Errorf("job: select recovery: %w", err)
	}
	type expired struct {
		id      string
		attempt int
		state   State
	}
	var records []expired
	for rows.Next() {
		var record expired
		if err := rows.Scan(&record.id, &record.attempt, &record.state); err != nil {
			_ = rows.Close()
			return 0, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, record := range records {
		state, code := Failed, "deadline_exceeded"
		if record.state == Running {
			state, code = Uncertain, "lease_expired"
		}
		_, err := tx.ExecContext(ctx, `UPDATE trest_job_queue SET state=$2,error_code=$3,attempt_token=NULL,lease_expires_at=NULL,
		finished_at=clock_timestamp(),updated_at=clock_timestamp(),dedupe_until=GREATEST(dedupe_until,clock_timestamp()+($4*interval '1 second')) WHERE id=$1`, record.id, state, code, durationSeconds(s.options.IdempotencyRetention))
		if err != nil {
			return 0, fmt.Errorf("job: recover: %w", err)
		}
		if err := event(ctx, tx, record.id, record.attempt, string(state), "", code); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("job: commit recovery: %w", err)
	}
	return len(records), nil
}

func event(ctx context.Context, tx *sql.Tx, id string, attempt int, kind, actor, detail string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO trest_job_event(job_id,attempt,kind,actor,detail) VALUES($1,$2,$3,$4,$5)`, id, attempt, kind, actor, detail)
	if err != nil {
		return fmt.Errorf("job: append event: %w", err)
	}
	return nil
}
