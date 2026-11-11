// Package schedule runs non-overlapping interval tasks during an app lifecycle.
// It is an in-process convenience; use durable jobs for retry or crash recovery.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mujhtech/trest"
)

// Task is one cooperative interval task. The next interval starts after a run
// finishes, so a slow callback never overlaps itself or queues missed ticks.
type Task struct {
	Name  string
	Every time.Duration
	// Timeout bounds each callback cooperatively. Defaults to Every.
	Timeout time.Duration
	Run     func(context.Context) error
	// OnError receives errors on the scheduler goroutine and must return promptly.
	OnError        func(error)
	RunImmediately bool
}

// New creates lifecycle hooks without starting a goroutine. The same component
// cannot be started twice concurrently or restarted while an old callback drains.
func New(task Task) (trest.Component, error) {
	if task.Name == "" || task.Every <= 0 || task.Run == nil {
		return nil, errors.New("schedule: name, positive interval, and callback are required")
	}
	if task.Timeout == 0 {
		task.Timeout = task.Every
	}
	if task.Timeout < 0 {
		return nil, errors.New("schedule: timeout must be positive")
	}
	runner := &runner{task: task}
	return trest.Lifecycle(runner.start, runner.stop), nil
}

type runner struct {
	mu     sync.Mutex
	task   Task
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *runner) start(parent context.Context) error {
	if parent == nil {
		return errors.New("schedule: nil lifecycle context")
	}
	if err := parent.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil {
		select {
		case <-r.done:
			r.done = nil
		default:
			return errors.New("schedule: task already running")
		}
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	r.cancel = cancel
	r.done = done
	go r.loop(ctx, done)
	return nil
}
func (r *runner) stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("schedule: nil shutdown context")
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("schedule: %s did not stop: %w", r.task.Name, ctx.Err())
	}
}
func (r *runner) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	delay := r.task.Every
	if r.task.RunImmediately {
		delay = 0
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}
		callCtx, cancel := context.WithTimeout(ctx, r.task.Timeout)
		err := r.call(callCtx)
		cancel()
		if err != nil && r.task.OnError != nil {
			r.report(err)
		}
		if ctx.Err() != nil {
			return
		}
		timer.Reset(r.task.Every)
	}
}
func (r *runner) call(ctx context.Context) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("schedule: task panicked")
		}
	}()
	err = r.task.Run(ctx)
	if err == nil {
		err = ctx.Err()
	}
	return err
}
func (r *runner) report(err error) { defer func() { _ = recover() }(); r.task.OnError(err) }
