package job

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/mujhtech/trest/operation"
)

// Worker runs one operation at a time; run multiple Workers for bounded parallelism.
// Run owns its heartbeat goroutine and waits for it before returning. Handlers must
// honor context cancellation; a Go goroutine cannot be forcibly terminated safely.
type Worker struct {
	store        *Store
	pollInterval time.Duration
}

func NewWorker(store *Store, pollInterval time.Duration) (*Worker, error) {
	if store == nil {
		return nil, errors.New("job: store is required")
	}
	if pollInterval == 0 {
		pollInterval = 250 * time.Millisecond
	}
	if pollInterval < time.Millisecond {
		return nil, errors.New("job: poll interval must be at least one millisecond")
	}
	return &Worker{store: store, pollInterval: pollInterval}, nil
}

// Run stops claiming as soon as ctx is cancelled, cancels in-flight work and waits
// for cooperative completion. Configure the application's shutdown deadline and
// terminate the process if a handler fails to cooperate; lease recovery is uncertain.
func (w *Worker) Run(ctx context.Context) error {
	timer := time.NewTimer(w.pollInterval)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		worked, err := w.RunOne(ctx)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		timer.Reset(w.pollInterval)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RunOne claims and processes at most one job. Handler failures are persisted and
// return nil; returned errors indicate queue/infrastructure failures, whose result
// may be uncertain. false,nil means there was no eligible work.
func (w *Worker) RunOne(ctx context.Context) (bool, error) {
	claimed, err := w.store.Claim(ctx)
	if errors.Is(err, ErrNoJob) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	runCtx := operation.WithPrincipal(ctx, claimed.Envelope.Principal)
	runCtx = operation.WithTrace(runCtx, claimed.Envelope.Trace)
	runCtx = operation.WithCorrelationID(runCtx, claimed.Envelope.Request.CorrelationID)
	var cancel context.CancelFunc
	if claimed.Envelope.Request.Deadline.IsZero() {
		runCtx, cancel = context.WithCancel(runCtx)
	} else {
		runCtx, cancel = context.WithDeadline(runCtx, claimed.Envelope.Request.Deadline)
	}
	defer cancel()
	stopHeartbeat := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(w.store.options.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeat:
				return
			case <-runCtx.Done():
				return
			case <-ticker.C:
				heartbeatCtx, stop := context.WithTimeout(runCtx, w.store.options.Lease/3)
				requested, err := w.store.Heartbeat(heartbeatCtx, claimed.ID, claimed.AttemptToken)
				stop()
				if err != nil || requested {
					cancel()
					return
				}
			}
		}
	}()
	outcome := w.execute(runCtx, claimed)
	close(stopHeartbeat)
	<-heartbeatDone
	// Persist terminal evidence even when the caller's deadline/cancellation fired.
	// This short, bounded cleanup context does not authorize further business work.
	finishCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	if err := w.store.Complete(finishCtx, claimed.ID, claimed.AttemptToken, outcome); err != nil {
		return true, err
	}
	return true, nil
}

func (w *Worker) execute(ctx context.Context, claimed Job) (outcome Outcome) {
	defer func() {
		if recover() != nil {
			outcome = Outcome{State: Uncertain, Code: "handler_panic"}
		}
	}()
	envelope := claimed.Envelope
	if envelope.Version != EnvelopeVersion || envelope.Request.PayloadVersion != w.store.version(envelope.Request.OperationID) {
		return Outcome{State: Failed, Code: "unsupported_payload_version"}
	}
	prepared, err := w.store.invoker.PrepareJSON(ctx, envelope.Request.OperationID, envelope.Request.Input)
	if err != nil {
		code := operation.CodeOf(err)
		if code == operation.Unavailable || code == operation.RateLimited {
			delay, delayErr := w.store.retryDelay(claimed.Attempts)
			if delayErr == nil {
				return Outcome{State: Pending, Code: string(code), RetryAfter: delay, NoHandlerExecuted: true}
			}
		}
		return Outcome{State: Failed, Code: string(code)}
	}
	digest := sha256.Sum256(append([]byte(envelope.Request.PayloadVersion+"\x00"), prepared...))
	if hex.EncodeToString(digest[:]) != envelope.InputHash {
		return Outcome{State: Failed, Code: "input_hash_mismatch"}
	}
	output, err := w.store.invoker.InvokeJSON(ctx, envelope.Request.OperationID, prepared)
	if err == nil {
		if len(output) > w.store.options.MaxOutputBytes || !json.Valid(output) {
			return Outcome{State: Uncertain, Code: "output_limit_or_encoding"}
		}
		return Outcome{State: Succeeded, Output: output}
	}
	code := operation.CodeOf(err)
	switch code {
	case operation.Unavailable, operation.RateLimited:
		if envelope.Request.RetrySafe {
			delay, err := w.store.retryDelay(claimed.Attempts)
			if err != nil {
				return Outcome{State: Uncertain, Code: "retry_randomness_failed"}
			}
			return Outcome{State: Pending, Code: string(code), RetryAfter: delay}
		}
	}
	return Outcome{State: Uncertain, Code: string(code)}
}

func (s *Store) retryDelay(attempt int) (time.Duration, error) {
	if attempt < 1 {
		return 0, errors.New("job: invalid attempt")
	}
	delay := s.options.BaseBackoff
	for i := 1; i < attempt && delay < s.options.MaxBackoff; i++ {
		if delay > s.options.MaxBackoff/2 {
			delay = s.options.MaxBackoff
		} else {
			delay *= 2
		}
	}
	if delay > s.options.MaxBackoff {
		delay = s.options.MaxBackoff
	}
	// Equal jitter keeps retries between half and all of the bounded backoff.
	half := delay / 2
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(delay-half)+1))
	if err != nil {
		return 0, fmt.Errorf("job: retry jitter: %w", err)
	}
	return half + time.Duration(jitter.Int64()), nil
}
