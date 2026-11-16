package randomid

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func countingSource() func([]byte) (int, error) {
	var sequence uint64
	return func(dst []byte) (int, error) {
		for offset := 0; offset < len(dst); offset += 16 {
			sequence++
			binary.BigEndian.PutUint64(dst[offset:offset+8], sequence)
			binary.BigEndian.PutUint64(dst[offset+8:offset+16], ^sequence)
		}
		return len(dst), nil
	}
}

func idSequence(t *testing.T, id string) uint64 {
	t.Helper()
	data, err := hex.DecodeString(id)
	if err != nil || len(data) != 16 {
		t.Fatalf("invalid 128-bit hexadecimal ID %q: %v", id, err)
	}
	sequence := binary.BigEndian.Uint64(data[:8])
	if binary.BigEndian.Uint64(data[8:]) != ^sequence {
		t.Fatalf("ID combined bytes from different entropy segments: %q", id)
	}
	return sequence
}

func TestIDRetainsStringsAcrossRefills(t *testing.T) {
	t.Parallel()
	g := &Generator{read: countingSource()}
	var retained []string
	for range 100 {
		id, err := g.ID()
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, id)
	}
	for index, id := range retained {
		if got := idSequence(t, id); got != uint64(index+1) {
			t.Fatalf("retained ID %d changed or reused entropy: got %d", index, got)
		}
	}
}

func TestIDConcurrentCallsConsumeSeparateEntropy(t *testing.T) {
	t.Parallel()
	const workers, perWorker = 16, 100
	g := &Generator{read: countingSource()}
	results := make(chan string, workers*perWorker)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				id, err := g.ID()
				if err != nil {
					t.Error(err)
					return
				}
				results <- id
			}
		}()
	}
	wg.Wait()
	close(results)
	seen := make(map[uint64]bool, workers*perWorker)
	for id := range results {
		sequence := idSequence(t, id)
		if sequence == 0 || sequence > workers*perWorker || seen[sequence] {
			t.Fatalf("reused or unexpected entropy segment %d", sequence)
		}
		seen[sequence] = true
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("got %d IDs, want %d", len(seen), workers*perWorker)
	}
}

func TestIDFailedRefillReturnsNoPartialIDAndRetries(t *testing.T) {
	t.Parallel()
	failure := errors.New("entropy unavailable")
	for _, test := range []struct {
		name string
		n    int
		err  error
		want error
	}{
		{name: "error", n: 0, err: failure, want: failure},
		{name: "partial error", n: 12, err: failure, want: failure},
		{name: "full buffer with error", n: 256, err: failure, want: failure},
		{name: "short success", n: 12, want: io.ErrUnexpectedEOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			success := countingSource()
			g := &Generator{read: func(dst []byte) (int, error) {
				calls++
				if calls == 1 {
					for index := range dst {
						dst[index] = 0xff
					}
					return test.n, test.err
				}
				return success(dst)
			}}
			if id, err := g.ID(); id != "" || !errors.Is(err, test.want) {
				t.Fatalf("failure returned ID=%q, err=%v", id, err)
			}
			id, err := g.ID()
			if err != nil || idSequence(t, id) != 1 {
				t.Fatalf("recovery returned ID=%q, err=%v", id, err)
			}
		})
	}
}

func TestIDPanicDuringRefillUnlocksAndDiscardsPartialBytes(t *testing.T) {
	t.Parallel()
	calls := 0
	success := countingSource()
	g := &Generator{read: func(dst []byte) (int, error) {
		calls++
		if calls == 1 {
			dst[0] = 0xff
			panic("test entropy panic")
		}
		return success(dst)
	}}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("entropy source panic was swallowed")
			}
		}()
		_, _ = g.ID()
	}()
	type result struct {
		id  string
		err error
	}
	finished := make(chan result, 1)
	go func() {
		id, err := g.ID()
		finished <- result{id, err}
	}()
	select {
	case got := <-finished:
		if got.err != nil || idSequence(t, got.id) != 1 {
			t.Fatalf("panic recovery returned ID=%q, err=%v", got.id, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("entropy source panic left generator locked")
	}
}

func TestNewAndZeroValueUseIndependentRandomEntropy(t *testing.T) {
	t.Parallel()
	var zero Generator
	seen := make(map[string]bool)
	for _, generator := range []*Generator{New(), New(), &zero} {
		for range 32 {
			id, err := generator.ID()
			if err != nil {
				t.Fatal(err)
			}
			data, err := hex.DecodeString(id)
			if err != nil || len(data) != 16 {
				t.Fatalf("invalid random ID %q: %v", id, err)
			}
			if seen[id] || id == "00000000000000000000000000000000" {
				t.Fatalf("random entropy was reused or uninitialized: %q", id)
			}
			seen[id] = true
		}
	}
}
