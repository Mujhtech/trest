package randomid

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPairRetainsIndependentIDsAcrossMixedRefills(t *testing.T) {
	t.Parallel()
	g := &Generator{read: countingSource()}
	var retained []string
	for index := range 100 {
		if index%3 == 0 {
			id, err := g.ID()
			if err != nil {
				t.Fatal(err)
			}
			retained = append(retained, id)
			continue
		}
		first, second, err := g.Pair()
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, first, second)
	}
	for index, id := range retained {
		if got := idSequence(t, id); got != uint64(index+1) {
			t.Fatalf("retained ID %d changed or reused entropy: got %d", index, got)
		}
	}
}

func TestPairConsumesConsecutiveIDsAtEveryRefillBoundary(t *testing.T) {
	t.Parallel()
	for warm := range 32 {
		t.Run(fmt.Sprintf("after_%d_IDs", warm), func(t *testing.T) {
			g := &Generator{read: countingSource()}
			for range warm {
				if _, err := g.ID(); err != nil {
					t.Fatal(err)
				}
			}
			first, second, err := g.Pair()
			if err != nil {
				t.Fatal(err)
			}
			next, err := g.ID()
			if err != nil {
				t.Fatal(err)
			}
			for offset, id := range []string{first, second, next} {
				if got, want := idSequence(t, id), uint64(warm+offset+1); got != want {
					t.Fatalf("ID %d: got entropy segment %d, want %d", offset, got, want)
				}
			}
		})
	}
}

func TestRetainedPairRemainsStableDuringConcurrentRefills(t *testing.T) {
	t.Parallel()
	g := &Generator{read: countingSource()}
	first, second, err := g.Pair()
	if err != nil {
		t.Fatal(err)
	}
	wantFirst, wantSecond := strings.Clone(first), strings.Clone(second)
	reading := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		close(reading)
		for range 4096 {
			if first != wantFirst || second != wantSecond {
				t.Error("retained pair changed during a later refill")
				return
			}
		}
	}()
	<-reading
	for range 1024 {
		if _, _, err := g.Pair(); err != nil {
			t.Error(err)
			break
		}
	}
	<-finished
	if first != wantFirst || second != wantSecond {
		t.Fatal("retained pair changed after later refills")
	}
}

func TestPairAndIDConcurrentCallsConsumeSeparateEntropy(t *testing.T) {
	t.Parallel()
	const workers, perWorker = 16, 64
	g := &Generator{read: countingSource()}
	type pair struct{ first, second string }
	results := make(chan pair, workers*perWorker)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range perWorker {
				if index%2 == 0 {
					id, err := g.ID()
					if err != nil {
						t.Error(err)
						return
					}
					results <- pair{first: id}
					continue
				}
				first, second, err := g.Pair()
				if err != nil {
					t.Error(err)
					return
				}
				results <- pair{first, second}
			}
		}()
	}
	wg.Wait()
	close(results)
	const want = workers * perWorker * 3 / 2
	seen := make(map[uint64]bool, want)
	for result := range results {
		first := idSequence(t, result.first)
		if result.second != "" && idSequence(t, result.second) != first+1 {
			t.Fatal("another call consumed entropy from inside a pair")
		}
		for _, id := range []string{result.first, result.second} {
			if id == "" {
				continue
			}
			sequence := idSequence(t, id)
			if sequence == 0 || sequence > want || seen[sequence] {
				t.Fatalf("reused or unexpected entropy segment %d", sequence)
			}
			seen[sequence] = true
		}
	}
	if len(seen) != want {
		t.Fatalf("got %d IDs, want %d", len(seen), want)
	}
}

func TestPairFailedRefillReturnsNeitherIDAndDiscardsPartialBytes(t *testing.T) {
	t.Parallel()
	failure := errors.New("entropy unavailable")
	for _, boundary := range []struct {
		name string
		warm int
		want uint64
	}{
		{name: "empty", want: 1},
		{name: "one ID remains", warm: 15, want: 17},
	} {
		for _, outcome := range []struct {
			name string
			n    int
			err  error
			want error
		}{
			{name: "error", err: failure, want: failure},
			{name: "partial error", n: 12, err: failure, want: failure},
			{name: "full buffer with error", n: 256, err: failure, want: failure},
			{name: "short success", n: 12, want: io.ErrUnexpectedEOF},
		} {
			t.Run(boundary.name+"/"+outcome.name, func(t *testing.T) {
				calls, failOn := 0, 1
				if boundary.warm > 0 {
					failOn = 2
				}
				success := countingSource()
				g := &Generator{read: func(dst []byte) (int, error) {
					calls++
					if calls == failOn {
						for index := range dst {
							dst[index] = 0xff
						}
						return outcome.n, outcome.err
					}
					return success(dst)
				}}
				for range boundary.warm {
					if _, err := g.ID(); err != nil {
						t.Fatal(err)
					}
				}
				first, second, err := g.Pair()
				if first != "" || second != "" || !errors.Is(err, outcome.want) {
					t.Fatalf("failure returned IDs=%q/%q, err=%v", first, second, err)
				}
				first, second, err = g.Pair()
				if err != nil || idSequence(t, first) != boundary.want || idSequence(t, second) != boundary.want+1 {
					t.Fatalf("recovery returned IDs=%q/%q, err=%v", first, second, err)
				}
			})
		}
	}
}

func TestPairPanicDuringRefillUnlocksAndDiscardsPartialBytes(t *testing.T) {
	t.Parallel()
	for _, boundary := range []struct {
		name string
		warm int
		want uint64
	}{
		{name: "empty", want: 1},
		{name: "one ID remains", warm: 15, want: 17},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			calls, panicOn := 0, 1
			if boundary.warm > 0 {
				panicOn = 2
			}
			success := countingSource()
			g := &Generator{read: func(dst []byte) (int, error) {
				calls++
				if calls == panicOn {
					dst[0] = 0xff
					panic("test entropy panic")
				}
				return success(dst)
			}}
			for range boundary.warm {
				if _, err := g.ID(); err != nil {
					t.Fatal(err)
				}
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Error("entropy source panic was swallowed")
					}
				}()
				_, _, _ = g.Pair()
			}()
			type result struct {
				first, second string
				err           error
			}
			finished := make(chan result, 1)
			go func() {
				first, second, err := g.Pair()
				finished <- result{first, second, err}
			}()
			select {
			case got := <-finished:
				if got.err != nil || idSequence(t, got.first) != boundary.want || idSequence(t, got.second) != boundary.want+1 {
					t.Fatalf("panic recovery returned IDs=%q/%q, err=%v", got.first, got.second, got.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("entropy source panic left generator locked")
			}
		})
	}
}

func TestPairNewAndZeroValueUseIndependentRandomEntropy(t *testing.T) {
	t.Parallel()
	var zero Generator
	seen := make(map[string]bool)
	for _, generator := range []*Generator{New(), New(), &zero} {
		for range 32 {
			first, second, err := generator.Pair()
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{first, second} {
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
}
