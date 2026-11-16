package trest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/operation"
)

func TestNativeJSONResponsePreservesEncodingLimitsAndHEAD(t *testing.T) {
	type output struct {
		Value string `json:"value"`
	}
	value := output{Value: strings.Repeat("é", 128)}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, method string
		limit        int64
		status       int
	}{
		{"exact encoded limit", "GET", int64(len(encoded)), 201},
		{"HEAD", "HEAD", int64(len(encoded)), 201},
		{"too large before writing", "GET", int64(len(encoded) - 1), 500},
	} {
		t.Run(test.name, func(t *testing.T) {
			var event trest.Event
			app, err := trest.New(trest.Limits(64, test.limit), trest.AfterResponse(func(e trest.Event) { event = e }), trest.Get("/", trest.Const(value)).Status(201))
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(test.method, "/", nil))
			if w.Code != test.status || event.Status != test.status {
				t.Fatalf("status=%d event=%+v", w.Code, event)
			}
			if test.status != 201 {
				if event.Code != operation.Internal || strings.Contains(w.Body.String(), value.Value) {
					t.Fatalf("invalid output leaked: %s event=%+v", w.Body, event)
				}
				return
			}
			want := string(encoded)
			if test.method == "HEAD" {
				want = ""
			}
			if w.Body.String() != want || w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
				t.Fatalf("body=%q header=%v", w.Body.String(), w.Header())
			}
		})
	}
}

func TestNativeJSONResponsesUseEachFreshValue(t *testing.T) {
	type output struct {
		Value string `json:"value"`
		Count int    `json:"count"`
	}
	var current output
	app, err := trest.New(trest.Get("/", func(context.Context, trest.Empty) (output, error) { return current, nil }))
	if err != nil {
		t.Fatal(err)
	}
	var retained []*httptest.ResponseRecorder
	var expected []string
	for index, text := range []string{"first", "<>&\u2028\u2029", "\xff", strings.Repeat("long", 9000), "last"} {
		current = output{Value: text, Count: index}
		encoded, err := json.Marshal(current)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		retained = append(retained, w)
		expected = append(expected, string(encoded))
		for i, previous := range retained {
			if previous.Body.String() != expected[i] {
				t.Fatalf("response %d changed after request %d", i, index)
			}
		}
	}
}

type failedJSONResponse struct{ *httptest.ResponseRecorder }

func (failedJSONResponse) Write([]byte) (int, error) { return 0, errors.New("transport failed") }

func TestNativeJSONWriterFailurePreservesOutcome(t *testing.T) {
	var event trest.Event
	app, err := trest.New(trest.AfterResponse(func(e trest.Event) { event = e }), trest.Get("/", trest.Const(struct{ Value string }{"value"})))
	if err != nil {
		t.Fatal(err)
	}
	w := failedJSONResponse{httptest.NewRecorder()}
	got := protocolPanic(func() { app.ServeHTTP(w, httptest.NewRequest("GET", "/", nil)) })
	if got != http.ErrAbortHandler || event.Code != operation.Internal || event.Status != 200 || w.Body.Len() != 0 {
		t.Fatalf("panic=%v event=%+v body=%s", got, event, w.Body)
	}
}
