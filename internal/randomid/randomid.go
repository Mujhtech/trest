// Package randomid generates independent 128-bit request and invocation IDs.
// Each generator owns its entropy buffer; there is no shared application state.
package randomid

import (
	"crypto/rand"
	"io"
	"sync"
	"sync/atomic"
)

const idsPerBlock = uint32(16)

// encodedBlock publishes immutable bytes and a monotonically increasing ID
// reservation index. A successful reservation never exceeds idsPerBlock.
type encodedBlock struct {
	encoded string
	next    atomic.Uint32
}

func (b *encodedBlock) reserve(count uint32) (uint32, bool) {
	for next := b.next.Load(); next <= idsPerBlock-count; next = b.next.Load() {
		if b.next.CompareAndSwap(next, next+count) {
			return next, true
		}
	}
	return 0, false
}

func (b *encodedBlock) id(index uint32) string {
	start := index * 32
	return b.encoded[start : start+32]
}

// Generator amortizes calls to crypto/rand and hexadecimal string allocations
// without replacing its entropy source. Each immutable encoded block holds 16
// IDs; retaining one ID can retain at most that 512-byte block. Its zero value is
// ready to use. A Generator must not be copied after use.
type Generator struct {
	current atomic.Pointer[encodedBlock]
	mu      sync.Mutex
	buffer  [256]byte
	// Tests can supply an instance-local source. Production always uses
	// crypto/rand.Read, including its platform-specific failure semantics.
	read func([]byte) (int, error)
}

// New constructs an independent generator without reading entropy or starting
// a goroutine. Entropy is requested lazily on the first ID and each refill.
func New() *Generator { return new(Generator) }

// ID returns 32 hexadecimal characters representing fresh 128-bit entropy.
// The string retains immutable storage and is unchanged by subsequent refills.
func (g *Generator) ID() (string, error) {
	if block := g.current.Load(); block != nil {
		if index, ok := block.reserve(1); ok {
			return block.id(index), nil
		}
	}
	first, _, err := g.takeSlow(1)
	return first, err
}

// Pair reserves two consecutive independent 128-bit IDs in one atomic step.
// A pair spanning blocks uses the refill lock so no other call can consume its
// second ID first. Failure returns neither ID and discards the consumed tail.
func (g *Generator) Pair() (string, string, error) {
	if block := g.current.Load(); block != nil {
		if index, ok := block.reserve(2); ok {
			return block.id(index), block.id(index + 1), nil
		}
	}
	return g.takeSlow(2)
}

func (g *Generator) takeSlow(count uint32) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// Another caller may have published a refill while this call waited.
	// Recheck and reserve it before considering another entropy read.
	if block := g.current.Load(); block != nil {
		if index, ok := block.reserve(count); ok {
			if count == 1 {
				return block.id(index), "", nil
			}
			return block.id(index), block.id(index + 1), nil
		}
		if count == 2 {
			// At most one ID remains after reserve(2) fails. Claim that tail
			// while holding the refill lock, then reserve the next block's first
			// ID before publishing it, preserving a pair's consecutive entropy.
			if index, ok := block.reserve(1); ok {
				first := block.id(index)
				next, err := g.refill()
				if err != nil {
					return "", "", err
				}
				next.next.Store(1)
				g.current.Store(next)
				return first, next.id(0), nil
			}
		}
	}
	next, err := g.refill()
	if err != nil {
		return "", "", err
	}
	// Reserve this caller's result before publication. All other callers see
	// only the unclaimed suffix; no fast-path call takes a partially read ID.
	next.next.Store(count)
	g.current.Store(next)
	if count == 1 {
		return next.id(0), "", nil
	}
	return next.id(0), next.id(1), nil
}

// refill runs under mu and publishes nothing. Until the complete read and
// encoding succeed, current remains the old exhausted block (or nil). Errors,
// short reads, and panics therefore leave no partially generated ID available.
func (g *Generator) refill() (*encodedBlock, error) {
	read := g.read
	if read == nil {
		read = rand.Read
	}
	n, err := read(g.buffer[:])
	if err != nil {
		return nil, err
	}
	if n != len(g.buffer) {
		return nil, io.ErrUnexpectedEOF
	}
	var encoded [512]byte
	encodeHexBlock(&encoded, &g.buffer)
	return &encodedBlock{encoded: string(encoded[:])}, nil
}
