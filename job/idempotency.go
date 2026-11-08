package job

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// DoEffect explicitly opens one transaction, executes application database work,
// and commits its idempotency result together. A concurrent duplicate waits for the
// transaction, then receives the original result. The callback may Publish an outbox
// entry through the supplied transaction. It must not commit/rollback that transaction.
//
// This guarantees atomicity only for writes made through tx to this database.
// External effects still require provider idempotency or reconciliation. A commit
// error is uncertain: retry this exact scoped key/input to discover the stored result.
func (s *Store) DoEffect(ctx context.Context, request Request, effect func(context.Context, *sql.Tx) (json.RawMessage, error)) (json.RawMessage, bool, error) {
	if request.IdempotencyKey == "" || effect == nil {
		return nil, false, errors.New("job: transactional effect requires idempotency key and callback")
	}
	envelope, err := s.prepare(ctx, request)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("job: begin idempotent effect: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var inserted string
	err = tx.QueryRowContext(ctx, `INSERT INTO trest_job_effect(tenant_id,operation_id,caller_scope,idempotency_key,principal_id,input_hash,expires_at)
	VALUES($1,$2,$3,$4,$5,$6,clock_timestamp()+($7*interval '1 second')) ON CONFLICT DO NOTHING RETURNING idempotency_key`,
		envelope.Principal.TenantID, request.OperationID, request.CallerScope, request.IdempotencyKey, envelope.Principal.ID, envelope.InputHash, durationSeconds(s.options.IdempotencyRetention)).Scan(&inserted)
	if errors.Is(err, sql.ErrNoRows) {
		var hash, principal string
		var output []byte
		if err := tx.QueryRowContext(ctx, `SELECT input_hash,principal_id,output FROM trest_job_effect
		WHERE tenant_id=$1 AND operation_id=$2 AND caller_scope=$3 AND idempotency_key=$4 FOR UPDATE`,
			envelope.Principal.TenantID, request.OperationID, request.CallerScope, request.IdempotencyKey).Scan(&hash, &principal, &output); err != nil {
			return nil, false, fmt.Errorf("job: read effect result: %w", err)
		}
		if hash != envelope.InputHash || principal != envelope.Principal.ID {
			return nil, false, ErrConflict
		}
		if output == nil {
			return nil, false, errors.New("job: effect result is incomplete; reconcile database state")
		}
		if err := tx.Commit(); err != nil {
			return nil, false, fmt.Errorf("job: finish duplicate lookup: %w", err)
		}
		return output, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("job: reserve effect key: %w", err)
	}
	output, err := effect(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if len(output) > s.options.MaxOutputBytes || !json.Valid(output) {
		return nil, false, errors.New("job: effect result exceeds limit or is invalid json")
	}
	_, err = tx.ExecContext(ctx, `UPDATE trest_job_effect SET output=$5::json,expires_at=clock_timestamp()+($6*interval '1 second')
	WHERE tenant_id=$1 AND operation_id=$2 AND caller_scope=$3 AND idempotency_key=$4`, envelope.Principal.TenantID, request.OperationID, request.CallerScope, request.IdempotencyKey, string(output), durationSeconds(s.options.IdempotencyRetention))
	if err != nil {
		return nil, false, fmt.Errorf("job: store effect result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("job: commit effect (outcome may be unknown): %w", err)
	}
	return output, false, nil
}

// CleanupEffects removes old completed effect keys. The application must size the
// retention window to exceed every possible retry/replay window before calling it.
func (s *Store) CleanupEffects(ctx context.Context, limit int) (int64, error) {
	if limit < 1 || limit > 1000 {
		return 0, errors.New("job: cleanup limit must be between 1 and 1000")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM trest_job_effect WHERE (tenant_id,operation_id,caller_scope,idempotency_key) IN
	(SELECT tenant_id,operation_id,caller_scope,idempotency_key FROM trest_job_effect WHERE output IS NOT NULL AND expires_at<clock_timestamp()
	ORDER BY expires_at FOR UPDATE SKIP LOCKED LIMIT $1)`, limit)
	if err != nil {
		return 0, fmt.Errorf("job: effect cleanup: %w", err)
	}
	return result.RowsAffected()
}
