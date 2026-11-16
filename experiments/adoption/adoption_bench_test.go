package adoption_test

import (
	"testing"

	"github.com/mujhtech/trest/experiments/adoption"
)

// BenchmarkDuplicateInvoice includes request construction and response recording.
// The fixture is warmed before timing, so every timed request is a duplicate.
func BenchmarkDuplicateInvoice(b *testing.B) {
	for _, factory := range adoption.Factories() {
		b.Run("framework="+factory.Name, func(b *testing.B) {
			fixture, err := factory.New()
			if err != nil {
				b.Fatal(err)
			}
			warm := request(fixture.Handler, "/invoices", "acme-test", adoption.Body)
			if warm.Code != 201 || fixture.Store.Count() != 1 {
				b.Fatalf("warmup: %d %s", warm.Code, warm.Body)
			}
			b.ReportAllocs()
			for b.Loop() {
				w := request(fixture.Handler, "/invoices", "acme-test", adoption.Body)
				if w.Code != 201 {
					b.Fatalf("request: %d %s", w.Code, w.Body)
				}
			}
			if fixture.Store.Count() != 1 {
				b.Fatal("duplicate request caused another write")
			}
		})
	}
}
