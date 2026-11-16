package randomid

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestConcurrentRefillPreservesConsecutivePairsAndUnusedEntropy(t *testing.T) {
	for _, warm := range []int{15, 16} {
		t.Run(fmt.Sprintf("after_%d_IDs", warm), func(t *testing.T) {
			const workers = 64
			refilling := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			readCalls := 0
			source := countingSource()
			g := &Generator{read: func(dst []byte) (int, error) {
				readCalls++
				if readCalls == 2 {
					// The replacement is deliberately incomplete while other
					// callers exhaust the old block and wait for this refill.
					dst[0] = 0xff
					close(refilling)
					<-release
				}
				return source(dst)
			}}
			for range warm {
				if _, err := g.ID(); err != nil {
					t.Fatal(err)
				}
			}
			old := g.current.Load()
			type result struct {
				first, second string
				err           error
			}
			firstPair := make(chan result, 1)
			go func() {
				first, second, err := g.Pair()
				firstPair <- result{first, second, err}
			}()
			select {
			case <-refilling:
			case <-time.After(5 * time.Second):
				t.Fatal("pair did not reach the controlled refill")
			}
			if g.current.Load() != old || old.next.Load() != idsPerBlock {
				t.Fatal("partially read replacement was published or the old tail remained available")
			}
			results := make(chan result, workers)
			var ready, done sync.WaitGroup
			ready.Add(workers)
			done.Add(workers)
			start := make(chan struct{})
			for worker := range workers {
				go func() {
					defer done.Done()
					ready.Done()
					<-start
					if worker%2 == 0 {
						id, err := g.ID()
						results <- result{first: id, err: err}
						return
					}
					first, second, err := g.Pair()
					results <- result{first, second, err}
				}()
			}
			ready.Wait()
			close(start)
			unblock()
			done.Wait()
			close(results)
			pair := <-firstPair
			if pair.err != nil || idSequence(t, pair.first) != uint64(warm+1) || idSequence(t, pair.second) != uint64(warm+2) {
				t.Fatalf("refill pair was interleaved: %+v", pair)
			}
			seen := map[uint64]bool{uint64(warm + 1): true, uint64(warm + 2): true}
			last := uint64(warm + 2 + workers*3/2)
			for result := range results {
				if result.err != nil {
					t.Fatal(result.err)
				}
				first := idSequence(t, result.first)
				if result.second != "" && idSequence(t, result.second) != first+1 {
					t.Fatal("concurrent pair lost consecutive entropy")
				}
				for _, id := range []string{result.first, result.second} {
					if id == "" {
						continue
					}
					sequence := idSequence(t, id)
					if sequence <= uint64(warm) || sequence > last || seen[sequence] {
						t.Fatalf("unused entropy was discarded or reused: segment %d", sequence)
					}
					seen[sequence] = true
				}
			}
			if len(seen) != 2+workers*3/2 || readCalls != int((last+15)/16) {
				t.Fatalf("refill accounting: %d IDs, %d reads; want %d IDs, %d reads", len(seen), readCalls, 2+workers*3/2, (last+15)/16)
			}
		})
	}
}
