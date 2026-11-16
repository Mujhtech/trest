package adoption_test

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mujhtech/trest/experiments/adoption"
)

func helloRequest(handler http.Handler) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/hello", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func checkHello(tb testing.TB, w *httptest.ResponseRecorder) {
	tb.Helper()
	if w.Code != http.StatusOK {
		tb.Fatalf("hello: %d %s", w.Code, w.Body)
	}
	contentType, _, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		tb.Fatalf("content type: %q (%v)", contentType, err)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		tb.Fatal(err)
	}
	if len(got) != 1 || got["message"] != "hello" {
		tb.Fatalf("unexpected response: %s", w.Body)
	}
}

func TestHelloFactories(t *testing.T) {
	for _, factory := range adoption.HelloFactories() {
		t.Run(factory.Name, func(t *testing.T) {
			fixture, err := factory.New()
			if err != nil {
				t.Fatal(err)
			}
			checkHello(t, helloRequest(fixture.Handler))
		})
	}
}
