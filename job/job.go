// Package job provides an explicitly started PostgreSQL job queue using database/sql.
// It does not import a driver, start background goroutines at import time, or apply
// migrations automatically. A claim lease fences queue state, not external effects.
package job

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"trest.local/trest/operation"
)

//go:embed migrations/001_init.sql
var migration string

// MigrationSQL returns the versioned schema for explicit application by deployment tooling.
func MigrationSQL() string { return migration }

const EnvelopeVersion = 1

// State is the persisted execution state. Uncertain requires reconciliation before retry.
type State string

const (
	Pending   State = "pending"
	Running   State = "running"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Cancelled State = "cancelled"
	Uncertain State = "uncertain"
)

var (
	ErrNoJob        = errors.New("job: no eligible job")
	ErrNotFound     = errors.New("job: not found")
	ErrConflict     = errors.New("job: idempotency key reused with different input or actor")
	ErrStaleAttempt = errors.New("job: stale or expired attempt")
	ErrInvalidState = errors.New("job: invalid state transition")
)

// Invoker is the enforced operation boundary. A frozen operation.Registry implements it.
type Invoker interface {
	PrepareJSON(context.Context, string, json.RawMessage) (json.RawMessage, error)
	InvokeJSON(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

// Options bounds persisted payloads, leases, retries and retention. Zero uses defaults.
// PayloadVersions maps operation IDs to accepted versions; absent entries accept "1".
// Callers must give incompatible payload meanings a new operation ID or version.
type Options struct {
	Lease                time.Duration
	MaxInputBytes        int
	MaxOutputBytes       int
	DefaultMaxAttempts   int
	BaseBackoff          time.Duration
	MaxBackoff           time.Duration
	Retention            time.Duration
	IdempotencyRetention time.Duration
	PayloadVersions      map[string]string
}

// Request describes business input and queue policy. Identity is captured from the
// trusted context, never extracted from Input. CallerScope is application-controlled.
// RetrySafe opts into automatic replay after transient failures; only enable it for
// idempotent handlers or effects protected by the application's idempotency protocol.
type Request struct {
	OperationID    string          `json:"operation_id"`
	PayloadVersion string          `json:"payload_version"`
	Input          json.RawMessage `json:"input"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	CallerScope    string          `json:"caller_scope"`
	CorrelationID  string          `json:"correlation_id,omitempty"`
	NotBefore      time.Time       `json:"not_before"`
	Deadline       time.Time       `json:"deadline"`
	MaxAttempts    int             `json:"max_attempts"`
	RetrySafe      bool            `json:"retry_safe"`
}

// Envelope is versioned durable input; principal and tenant are separate metadata.
type Envelope struct {
	Version   int                    `json:"version"`
	Request   Request                `json:"request"`
	Principal operation.Principal    `json:"principal"`
	Trace     operation.TraceContext `json:"trace_link"`
	InputHash string                 `json:"input_hash"`
}

// Job is a queue record. Treat Input and Output as private application data. Get and
// Events are administrative APIs: the application must authorize operator access.
type Job struct {
	ID              string
	Envelope        Envelope
	State           State
	Attempts        int
	AvailableAt     time.Time
	LeaseExpiresAt  time.Time
	AttemptToken    string
	CancelRequested bool
	Output          json.RawMessage
	ErrorCode       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	FinishedAt      time.Time
}

// Event contains redacted execution history. Detail contains codes or operator
// reasons, never exception messages or business input. Reasons must be safe to retain.
type Event struct {
	Sequence  int64
	JobID     string
	Attempt   int
	Kind      string
	Actor     string
	Detail    string
	CreatedAt time.Time
}

// Store uses PostgreSQL-specific SQL while keeping the driver dependency optional.
// Construct once, share safely, and keep its DB open until workers have drained.
type Store struct {
	db      *sql.DB
	invoker Invoker
	options Options
}

func New(db *sql.DB, invoker Invoker, options Options) (*Store, error) {
	if db == nil || invoker == nil {
		return nil, errors.New("job: db and invoker are required")
	}
	if options.Lease == 0 {
		options.Lease = 30 * time.Second
	}
	if options.MaxInputBytes == 0 {
		options.MaxInputBytes = 1 << 20
	}
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = 1 << 20
	}
	if options.DefaultMaxAttempts == 0 {
		options.DefaultMaxAttempts = 3
	}
	if options.BaseBackoff == 0 {
		options.BaseBackoff = time.Second
	}
	if options.MaxBackoff == 0 {
		options.MaxBackoff = time.Minute
	}
	if options.Retention == 0 {
		options.Retention = 7 * 24 * time.Hour
	}
	if options.IdempotencyRetention == 0 {
		options.IdempotencyRetention = 7 * 24 * time.Hour
	}
	if options.Lease < 30*time.Millisecond || options.Lease > 24*time.Hour || options.MaxInputBytes < 1 || options.MaxOutputBytes < 1 || options.DefaultMaxAttempts < 1 || options.DefaultMaxAttempts > 100 || options.BaseBackoff < time.Millisecond || options.MaxBackoff < options.BaseBackoff || options.MaxBackoff > 24*time.Hour || options.Retention < time.Second || options.IdempotencyRetention < time.Second {
		return nil, errors.New("job: invalid bounds, retry policy, lease, or retention options")
	}
	versions := make(map[string]string, len(options.PayloadVersions))
	for id, version := range options.PayloadVersions {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(version) == "" {
			return nil, errors.New("job: payload versions must not be empty")
		}
		versions[id] = version
	}
	options.PayloadVersions = versions
	return &Store{db: db, invoker: invoker, options: options}, nil
}

func (s *Store) version(id string) string {
	if version, ok := s.options.PayloadVersions[id]; ok {
		return version
	}
	return "1"
}

func (s *Store) prepare(ctx context.Context, request Request) (Envelope, error) {
	if strings.TrimSpace(request.OperationID) == "" || request.PayloadVersion != s.version(request.OperationID) || strings.TrimSpace(request.CallerScope) == "" {
		return Envelope{}, errors.New("job: operation, supported payload version, and caller scope are required")
	}
	for _, value := range []string{request.OperationID, request.PayloadVersion, request.CallerScope, request.IdempotencyKey, request.CorrelationID} {
		if len(value) > 512 || strings.ContainsRune(value, '\x00') {
			return Envelope{}, errors.New("job: metadata must be at most 512 bytes and contain no nul")
		}
	}
	if len(request.Input) > s.options.MaxInputBytes {
		return Envelope{}, errors.New("job: input exceeds size limit")
	}
	if request.MaxAttempts == 0 {
		request.MaxAttempts = s.options.DefaultMaxAttempts
	}
	if request.MaxAttempts < 1 || request.MaxAttempts > 100 {
		return Envelope{}, errors.New("job: max attempts must be between 1 and 100")
	}
	if !request.Deadline.IsZero() && !request.NotBefore.IsZero() && !request.Deadline.After(request.NotBefore) {
		return Envelope{}, errors.New("job: deadline must follow scheduled time")
	}
	input, err := s.invoker.PrepareJSON(ctx, request.OperationID, request.Input)
	if err != nil {
		return Envelope{}, fmt.Errorf("job: prepare operation: %w", err)
	}
	if len(input) > s.options.MaxInputBytes || !json.Valid(input) {
		return Envelope{}, errors.New("job: prepared input exceeds bound or is invalid json")
	}
	request.Input = append(json.RawMessage(nil), input...)
	principal, _ := operation.PrincipalFromContext(ctx)
	digest := sha256.Sum256(append([]byte(request.PayloadVersion+"\x00"), input...))
	return Envelope{Version: EnvelopeVersion, Request: request, Principal: principal, Trace: operation.TraceFromContext(ctx), InputHash: hex.EncodeToString(digest[:])}, nil
}

func newID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("job: random id: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func durationSeconds(d time.Duration) float64 { return d.Seconds() }
