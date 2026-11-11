//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mujhtech/trest/job"
	"github.com/mujhtech/trest/operation"
)

type input struct {
	ResourceTenant string `json:"resource_tenant"`
	Value          int    `json:"value"`
}
type output struct {
	Value int `json:"value"`
}

func database(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TREST_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set TREST_TEST_POSTGRES_DSN to an isolated trest_jobs_test database")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	if config.Database != "trest_jobs_test" {
		t.Fatal("integration tests require database name trest_jobs_test")
	}
	admin := stdlib.OpenDB(*config)
	admin.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "trest_it_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	config.RuntimeParams["search_path"] = name
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(6)
	db.SetMaxIdleConns(2)
	t.Cleanup(func() {
		_ = db.Close()
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error(err)
		}
		_ = admin.Close()
	})
	if _, err := db.ExecContext(ctx, job.MigrationSQL()); err != nil {
		t.Fatal(err)
	}
	return db
}

func registry(t *testing.T, handler func(context.Context, input) (output, error), denied *atomic.Bool) *operation.Registry {
	t.Helper()
	r := operation.NewRegistry(operation.AuthorizerFunc(func(_ context.Context, p operation.Principal, permission string, value any) error {
		in := value.(input)
		if permission != "test:write" || p.ID == "" || p.TenantID != in.ResourceTenant || (denied != nil && denied.Load()) {
			return operation.NewError(operation.Forbidden, "permission denied", nil)
		}
		return nil
	}))
	_, err := operation.Register(r, operation.Definition[input, output]{ID: "test.write.v1", Summary: "Write test value", Access: operation.Permission("test:write")}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Freeze(); err != nil {
		t.Fatal(err)
	}
	return r
}

func store(t *testing.T, db *sql.DB, r *operation.Registry, options job.Options) *job.Store {
	t.Helper()
	s, err := job.New(db, r, options)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func actor() context.Context {
	return operation.WithPrincipal(context.Background(), operation.Principal{ID: "operator-a", TenantID: "tenant-a"})
}
func request(key string) job.Request {
	return job.Request{OperationID: "test.write.v1", PayloadVersion: "1", Input: json.RawMessage(`{"resource_tenant":"tenant-a","value":42}`), CallerScope: "tests", IdempotencyKey: key}
}
func enqueue(t *testing.T, s *job.Store, key string) job.Job {
	t.Helper()
	j, _, err := s.Enqueue(actor(), request(key))
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func worker(t *testing.T, s *job.Store) *job.Worker {
	t.Helper()
	w, err := job.NewWorker(s, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func runOne(t *testing.T, w *job.Worker) {
	t.Helper()
	worked, err := w.RunOne(context.Background())
	if err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
}
func get(t *testing.T, s *job.Store, id string) job.Job {
	t.Helper()
	j, err := s.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestConcurrentDuplicateEnqueue(t *testing.T) {
	t.Parallel()
	db := database(t)
	r := registry(t, func(_ context.Context, in input) (output, error) { return output{in.Value}, nil }, nil)
	s := store(t, db, r, job.Options{})
	const count = 16
	ids := make(chan string, count)
	errs := make(chan error, count)
	var created atomic.Int32
	var group sync.WaitGroup
	for range count {
		group.Add(1)
		go func() {
			defer group.Done()
			j, newJob, err := s.Enqueue(actor(), request("same-key"))
			if err != nil {
				errs <- err
				return
			}
			if newJob {
				created.Add(1)
			}
			ids <- j.ID
		}()
	}
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("duplicate idempotency key produced multiple jobs")
		}
	}
	if created.Load() != 1 {
		t.Fatalf("created %d jobs", created.Load())
	}
	changed := request("same-key")
	changed.Input = json.RawMessage(`{"resource_tenant":"tenant-a","value":43}`)
	if _, _, err := s.Enqueue(actor(), changed); !errors.Is(err, job.ErrConflict) {
		t.Fatalf("changed input err=%v", err)
	}
	changed = request("same-key")
	changed.CallerScope = "another-caller"
	if _, fresh, err := s.Enqueue(actor(), changed); err != nil || !fresh {
		t.Fatalf("different scope fresh=%v err=%v", fresh, err)
	}
}

func TestWorkerAuthorizationTenantAndTrace(t *testing.T) {
	t.Parallel()
	db := database(t)
	var denied atomic.Bool
	var called atomic.Int32
	r := registry(t, func(ctx context.Context, in input) (output, error) {
		called.Add(1)
		p, ok := operation.PrincipalFromContext(ctx)
		if !ok || p.ID != "operator-a" || p.TenantID != "tenant-a" {
			return output{}, errors.New("identity was not preserved")
		}
		trace := operation.TraceFromContext(ctx)
		if operation.CorrelationID(ctx) != "job-correlation" {
			return output{}, errors.New("correlation was not preserved")
		}
		if trace.TraceID != "job-trace" {
			return output{}, errors.New("trace was not preserved")
		}
		return output{in.Value}, nil
	}, &denied)
	s := store(t, db, r, job.Options{})
	ctx := operation.WithTrace(actor(), operation.TraceContext{TraceID: "job-trace", SpanID: "publisher-span"})
	req := request("revoke")
	req.CorrelationID = "job-correlation"
	j, _, err := s.Enqueue(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	denied.Store(true)
	runOne(t, worker(t, s))
	if got := get(t, s, j.ID); got.State != job.Failed || got.ErrorCode != "forbidden" || called.Load() != 0 {
		t.Fatalf("revoked job=%+v called=%d", got, called.Load())
	}
	denied.Store(false)
	req = request("allowed")
	req.CorrelationID = "job-correlation"
	j, _, err = s.Enqueue(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	runOne(t, worker(t, s))
	if got := get(t, s, j.ID); got.State != job.Succeeded || called.Load() != 1 {
		t.Fatalf("job=%+v called=%d", got, called.Load())
	}
	bad := request("spoofed")
	bad.Input = json.RawMessage(`{"resource_tenant":"tenant-b","value":42}`)
	if _, _, err := s.Enqueue(actor(), bad); operation.CodeOf(err) != operation.Forbidden {
		t.Fatalf("cross-tenant input accepted: %v", err)
	}
}

func TestLeaseExpiryFencesStaleAttempt(t *testing.T) {
	t.Parallel()
	db := database(t)
	r := registry(t, func(_ context.Context, in input) (output, error) { return output{in.Value}, nil }, nil)
	s := store(t, db, r, job.Options{Lease: time.Second})
	j := enqueue(t, s, "lease")
	claim, err := s.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Fault injection advances only this fixture's lease, without timing dependence.
	if _, err := db.Exec(`UPDATE trest_job_queue SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(context.Background(), j.ID, claim.AttemptToken); !errors.Is(err, job.ErrStaleAttempt) {
		t.Fatalf("stale heartbeat=%v", err)
	}
	if err := s.Complete(context.Background(), j.ID, claim.AttemptToken, job.Outcome{State: job.Succeeded, Output: json.RawMessage(`{"value":42}`)}); !errors.Is(err, job.ErrStaleAttempt) {
		t.Fatalf("expired completion=%v", err)
	}
	if n, err := s.Recover(context.Background(), 10); err != nil || n != 1 {
		t.Fatalf("recover n=%d err=%v", n, err)
	}
	if got := get(t, s, j.ID); got.State != job.Uncertain {
		t.Fatalf("expired lease state=%s", got.State)
	}
	if err := s.Retry(context.Background(), j.ID, job.RetryRequest{Actor: "admin", Reason: "Confirmed no external effect", AdditionalAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.AttemptToken == claim.AttemptToken {
		t.Fatal("attempt token was reused")
	}
	if err := s.Complete(context.Background(), j.ID, claim.AttemptToken, job.Outcome{State: job.Succeeded, Output: json.RawMessage(`{"value":99}`)}); !errors.Is(err, job.ErrStaleAttempt) {
		t.Fatalf("old attempt accepted: %v", err)
	}
	if err := s.Complete(context.Background(), j.ID, fresh.AttemptToken, job.Outcome{State: job.Succeeded, Output: json.RawMessage(`{"value":42}`)}); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(context.Background(), j.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var audited bool
	for _, e := range events {
		if e.Kind == "manual_retry" && e.Actor == "admin" {
			audited = true
		}
	}
	if !audited {
		t.Fatal("manual retry was not audited")
	}
}

func TestRetriesExhaustionAndUnknownEffects(t *testing.T) {
	t.Parallel()
	db := database(t)
	var called atomic.Int32
	r := registry(t, func(context.Context, input) (output, error) {
		called.Add(1)
		return output{}, operation.NewError(operation.Unavailable, "try later", errors.New("private credential must not persist"))
	}, nil)
	s := store(t, db, r, job.Options{BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	req := request("retry-safe")
	req.MaxAttempts = 2
	req.RetrySafe = true
	j, _, err := s.Enqueue(actor(), req)
	if err != nil {
		t.Fatal(err)
	}
	w := worker(t, s)
	runOne(t, w)
	if got := get(t, s, j.ID); got.State != job.Pending {
		t.Fatalf("first transient state=%s", got.State)
	}
	if _, err := db.Exec(`UPDATE trest_job_queue SET available_at=clock_timestamp() WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	runOne(t, w)
	if got := get(t, s, j.ID); got.State != job.Failed || got.ErrorCode != "retry_exhausted" || got.Attempts != 2 {
		t.Fatalf("exhausted=%+v", got)
	}
	unknown := enqueue(t, s, "unsafe-retry")
	runOne(t, w)
	if got := get(t, s, unknown.ID); got.State != job.Uncertain {
		t.Fatalf("unsafe retry state=%s", got.State)
	}
	events, err := s.Events(context.Background(), unknown.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e.Detail, "credential") {
			t.Fatal("private error persisted")
		}
	}
}

func TestOutboxAtomicCommitAndRollback(t *testing.T) {
	t.Parallel()
	db := database(t)
	if _, err := db.Exec(`CREATE TABLE business_changes (id text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	r := registry(t, func(_ context.Context, in input) (output, error) { return output{in.Value}, nil }, nil)
	s := store(t, db, r, job.Options{})
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO business_changes VALUES('rolled-back')`); err != nil {
		t.Fatal(err)
	}
	rolled, err := s.Publish(actor(), tx, request("rolled-back"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Outbox(context.Background(), rolled); !errors.Is(err, job.ErrNotFound) {
		t.Fatalf("rolled back outbox survived: %v", err)
	}
	var changes int
	if err := db.QueryRow(`SELECT count(*) FROM business_changes`).Scan(&changes); err != nil || changes != 0 {
		t.Fatalf("business rollback changes=%d err=%v", changes, err)
	}
	if _, err := s.DispatchOne(context.Background()); !errors.Is(err, job.ErrNoJob) {
		t.Fatalf("rollback published a job: %v", err)
	}
	tx, err = db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO business_changes VALUES('committed')`); err != nil {
		t.Fatal(err)
	}
	outboxID, err := s.Publish(actor(), tx, request("committed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Simulated process restart: a fresh store picks up the committed outbox.
	restarted := store(t, db, r, job.Options{})
	jobID, err := restarted.DispatchOne(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	status, err := restarted.Outbox(context.Background(), outboxID)
	if err != nil || status.State != "dispatched" || status.JobID != jobID {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	// An uncertain publisher may repeat delivery: the job's scoped key deduplicates it.
	tx, err = db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(actor(), tx, request("committed")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	repeated, err := s.DispatchOne(context.Background())
	if err != nil || repeated != jobID {
		t.Fatalf("repeat id=%s err=%v", repeated, err)
	}
}

func TestCrashAfterEffectBeforeCompletion(t *testing.T) {
	t.Parallel()
	db := database(t)
	if _, err := db.Exec(`CREATE TABLE remote_effects (key text PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	r := registry(t, func(_ context.Context, in input) (output, error) { return output{in.Value}, nil }, nil)
	s := store(t, db, r, job.Options{})
	j := enqueue(t, s, "effect-crash")
	claim, err := s.Claim(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Fault: the remote/database effect commits, then the process dies before Complete.
	if _, err := db.Exec(`INSERT INTO remote_effects VALUES($1)`, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE trest_job_queue SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recover(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, j.ID); got.State != job.Uncertain {
		t.Fatalf("lost completion state=%s", got.State)
	}
	if _, err := s.Claim(context.Background()); !errors.Is(err, job.ErrNoJob) {
		t.Fatal("uncertain effect was automatically replayed")
	}
	if err := s.Resolve(context.Background(), j.ID, "admin", "Provider confirms original success", job.Outcome{State: job.Succeeded, Output: json.RawMessage(`{"value":42}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(context.Background(), j.ID, claim.AttemptToken, job.Outcome{State: job.Failed, Code: "late"}); !errors.Is(err, job.ErrStaleAttempt) {
		t.Fatal("stale worker overwrote reconciled result")
	}
	var effects int
	if err := db.QueryRow(`SELECT count(*) FROM remote_effects`).Scan(&effects); err != nil || effects != 1 {
		t.Fatalf("effects=%d err=%v", effects, err)
	}
}

func TestCancellationDeadlineVersionAndRetention(t *testing.T) {
	t.Parallel()
	db := database(t)
	var called atomic.Int32
	r := registry(t, func(_ context.Context, in input) (output, error) { called.Add(1); return output{in.Value}, nil }, nil)
	s := store(t, db, r, job.Options{Retention: time.Second, IdempotencyRetention: time.Hour})
	cancelled := enqueue(t, s, "cancelled")
	if err := s.Cancel(context.Background(), cancelled.ID, "admin", "No longer required"); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, cancelled.ID); got.State != job.Cancelled {
		t.Fatal(got.State)
	}
	req := request("expired")
	req.Deadline = time.Now().Add(-time.Second)
	expired, _, err := s.Enqueue(actor(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recover(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, expired.ID); got.State != job.Failed || got.ErrorCode != "deadline_exceeded" {
		t.Fatalf("expired=%+v", got)
	}
	versioned := enqueue(t, s, "versioned")
	upgraded := store(t, db, r, job.Options{PayloadVersions: map[string]string{"test.write.v1": "2"}})
	runOne(t, worker(t, upgraded))
	if got := get(t, s, versioned.ID); got.State != job.Failed || got.ErrorCode != "unsupported_payload_version" {
		t.Fatalf("versioned=%+v", got)
	}
	if called.Load() != 0 {
		t.Fatal("cancelled/expired/unsupported jobs invoked handler")
	}
	if _, err := db.Exec(`UPDATE trest_job_queue SET finished_at=clock_timestamp()-interval '1 day' WHERE id=$1`, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.Cleanup(context.Background(), 100); err != nil || deleted != 0 {
		t.Fatalf("dedupe window ignored deleted=%d err=%v", deleted, err)
	}
	if _, err := db.Exec(`UPDATE trest_job_queue SET dedupe_until=clock_timestamp()-interval '1 second' WHERE id=$1`, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.Cleanup(context.Background(), 100); err != nil || deleted != 1 {
		t.Fatalf("cleanup deleted=%d err=%v", deleted, err)
	}
}

func TestWorkerHeartbeatAndRunningCancellation(t *testing.T) {
	t.Parallel()
	db := database(t)
	started := make(chan struct{})
	stopped := make(chan struct{})
	r := registry(t, func(ctx context.Context, _ input) (output, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return output{}, ctx.Err()
	}, nil)
	s := store(t, db, r, job.Options{Lease: 150 * time.Millisecond})
	j := enqueue(t, s, "running-cancel")
	result := make(chan error, 1)
	go func() { _, err := worker(t, s).RunOne(context.Background()); result <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	// Exercise renewal beyond the initial lease before requesting cancellation.
	time.Sleep(300 * time.Millisecond)
	if got := get(t, s, j.ID); got.State != job.Running || !got.LeaseExpiresAt.After(time.Now()) {
		t.Fatalf("lease not renewed: %+v", got)
	}
	if err := s.Cancel(context.Background(), j.ID, "admin", "Stop requested"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not observe cancellation")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finish")
	}
	if got := get(t, s, j.ID); got.State != job.Uncertain {
		t.Fatalf("running cancellation claimed no effects: %s", got.State)
	}
}

func TestTransactionalEffectIdempotency(t *testing.T) {
	t.Parallel()
	db := database(t)
	if _, err := db.Exec(`CREATE TABLE invoices (id text PRIMARY KEY,amount integer NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	r := registry(t, func(_ context.Context, in input) (output, error) { return output{in.Value}, nil }, nil)
	s := store(t, db, r, job.Options{})
	var effects atomic.Int32
	effect := func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		effects.Add(1)
		if _, err := tx.ExecContext(ctx, `INSERT INTO invoices VALUES('invoice-1',42)`); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"value":42}`), nil
	}
	var group sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			out, _, err := s.DoEffect(actor(), request("effect-once"), effect)
			if err != nil {
				errs <- err
				return
			}
			var got output
			if err := json.Unmarshal(out, &got); err != nil || got.Value != 42 {
				errs <- errors.New("invalid effect result")
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if effects.Load() != 1 {
		t.Fatalf("effect ran %d times", effects.Load())
	}
	changed := request("effect-once")
	changed.Input = json.RawMessage(`{"resource_tenant":"tenant-a","value":99}`)
	if _, _, err := s.DoEffect(actor(), changed, effect); !errors.Is(err, job.ErrConflict) {
		t.Fatalf("effect key mismatch accepted: %v", err)
	}
	// Crash/error before commit rolls back the business write and reservation together.
	failing := func(ctx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO invoices VALUES('rolled-back',1)`); err != nil {
			return nil, err
		}
		return nil, errors.New("simulated crash before commit")
	}
	if _, _, err := s.DoEffect(actor(), request("effect-rollback"), failing); err == nil {
		t.Fatal("fault returned success")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM invoices WHERE id='rolled-back'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback count=%d err=%v", count, err)
	}
	// A lost acknowledgement after commit is recovered without executing the callback.
	_, replayed, err := s.DoEffect(actor(), request("effect-once"), effect)
	if err != nil || !replayed || effects.Load() != 1 {
		t.Fatalf("after commit replay=%v effects=%d err=%v", replayed, effects.Load(), err)
	}
}

type unavailablePreflight struct {
	invoker     *operation.Registry
	unavailable atomic.Bool
}

func (i *unavailablePreflight) PrepareJSON(ctx context.Context, id string, input json.RawMessage) (json.RawMessage, error) {
	if i.unavailable.Load() {
		return nil, operation.NewError(operation.Unavailable, "temporary authorization backend outage", nil)
	}
	return i.invoker.PrepareJSON(ctx, id, input)
}
func (i *unavailablePreflight) InvokeJSON(ctx context.Context, id string, input json.RawMessage) (json.RawMessage, error) {
	return i.invoker.InvokeJSON(ctx, id, input)
}

func TestPreflightRetriesBeforeHandler(t *testing.T) {
	t.Parallel()
	db := database(t)
	var called atomic.Int32
	r := registry(t, func(_ context.Context, in input) (output, error) { called.Add(1); return output{in.Value}, nil }, nil)
	bridge := &unavailablePreflight{invoker: r}
	s, err := job.New(db, bridge, job.Options{BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	j := enqueue(t, s, "preflight")
	bridge.unavailable.Store(true)
	runOne(t, worker(t, s))
	if got := get(t, s, j.ID); got.State != job.Pending || called.Load() != 0 {
		t.Fatalf("preflight=%+v calls=%d", got, called.Load())
	}
	bridge.unavailable.Store(false)
	if _, err := db.Exec(`UPDATE trest_job_queue SET available_at=clock_timestamp() WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	runOne(t, worker(t, s))
	if got := get(t, s, j.ID); got.State != job.Succeeded || called.Load() != 1 {
		t.Fatalf("recovered=%+v calls=%d", got, called.Load())
	}
}

func TestHandlerErrorAfterEffectIsUncertain(t *testing.T) {
	t.Parallel()
	db := database(t)
	if _, err := db.Exec(`CREATE TABLE committed_effects (id integer PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	r := registry(t, func(ctx context.Context, _ input) (output, error) {
		if _, err := db.ExecContext(ctx, `INSERT INTO committed_effects VALUES(1)`); err != nil {
			return output{}, err
		}
		return output{}, operation.NewError(operation.Conflict, "downstream conflict", nil)
	}, nil)
	s := store(t, db, r, job.Options{})
	j := enqueue(t, s, "effect-then-conflict")
	runOne(t, worker(t, s))
	if got := get(t, s, j.ID); got.State != job.Uncertain || got.ErrorCode != "conflict" {
		t.Fatalf("post-effect error=%+v", got)
	}
	if _, err := db.Exec(`UPDATE trest_job_queue SET finished_at=clock_timestamp()-interval '30 days',dedupe_until=clock_timestamp()-interval '1 day' WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.Cleanup(context.Background(), 100); err != nil || deleted != 0 {
		t.Fatalf("uncertain evidence deleted=%d err=%v", deleted, err)
	}
}
