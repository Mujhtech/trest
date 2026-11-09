// Package server holds the examples' explicit HTTP lifecycle policy.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Run serves until ctx is canceled or the listener fails. Shutdown gives in-flight
// requests five seconds to finish, then closes remaining connections.
func Run(ctx context.Context, address string, handler http.Handler) error {
	server := &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	completed := make(chan error, 1)
	go func() { completed <- server.ListenAndServe() }()
	select {
	case err := <-completed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			closeErr := server.Close()
			serveErr := <-completed
			if errors.Is(serveErr, http.ErrServerClosed) {
				serveErr = nil
			}
			return errors.Join(fmt.Errorf("shutdown: %w", err), closeErr, serveErr)
		}
		if err := <-completed; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
