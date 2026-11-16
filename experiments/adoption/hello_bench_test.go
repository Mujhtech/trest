package adoption_test

import (
	"net/http"
	"testing"

	"github.com/mujhtech/trest/experiments/adoption"
)

// BenchmarkHelloJSON measures native routing and JSON response serialization,
// including fresh httptest request and recorder creation in every iteration.
func BenchmarkHelloJSON(b *testing.B) {
	for _, factory := range adoption.HelloFactories() {
		b.Run("framework="+factory.Name, func(b *testing.B) {
			fixture, err := factory.New()
			if err != nil {
				b.Fatal(err)
			}
			checkHello(b, helloRequest(fixture.Handler))
			b.ReportAllocs()
			for b.Loop() {
				w := helloRequest(fixture.Handler)
				if w.Code != http.StatusOK {
					b.Fatalf("hello: %d %s", w.Code, w.Body)
				}
			}
		})
	}
}
