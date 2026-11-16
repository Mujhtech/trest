// Package spanreserve carries a single-use invocation ID from an HTTP request
// into its first operation. The reservation belongs to the request, not a pool.
package spanreserve

import (
	"context"
	"sync/atomic"
)

type key struct{}

// Reservation holds an immutable ID and permits one invocation to consume it.
// It may be embedded in request state and must not be copied after first use.
// Its zero value is an empty reservation.
type Reservation struct {
	id   string
	used atomic.Bool
}

// New prepares a reservation before its containing request is published.
func New(id string) Reservation { return Reservation{id: id} }

// IsKey identifies the private metadata lookup without exposing the key.
func IsKey(k any) bool {
	_, ok := k.(key)
	return ok
}

// Take returns the reserved ID once, or an empty string when none remains.
// Context wrappers should return their *Reservation for this package's key.
// Concurrent and nested invocations share the same atomic claim.
func Take(ctx context.Context) string {
	reservation, _ := ctx.Value(key{}).(*Reservation)
	if reservation == nil || !reservation.used.CompareAndSwap(false, true) {
		return ""
	}
	return reservation.id
}
