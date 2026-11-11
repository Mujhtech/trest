package trest_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/operation"
)

type stringResponse struct {
	*httptest.ResponseRecorder
	calls int
	err   error
}

func (w *stringResponse) WriteString(s string) (int, error) {
	w.calls++
	if w.err != nil {
		return 0, w.err
	}
	return w.ResponseRecorder.WriteString(s)
}

func TestPlainTextStringWritesPreserveByteLimitsAndHEAD(t *testing.T) {
	for _, tc := range []struct {
		name, method  string
		limit         int64
		status, calls int
	}{
		{"exact byte limit", "GET", 256, 201, 1},
		{"HEAD", "HEAD", 256, 201, 0},
		{"UTF8 bytes exceed limit", "GET", 256, 500, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event trest.Event
			text := strings.Repeat("é", 128)
			if tc.status == 500 {
				text += "é"
			}
			app, err := trest.New(trest.Limits(64, tc.limit), trest.AfterResponse(func(e trest.Event) { event = e }), trest.Get("/", trest.Const(namedText(text))).Status(201))
			if err != nil {
				t.Fatal(err)
			}
			w := &stringResponse{ResponseRecorder: httptest.NewRecorder()}
			app.ServeHTTP(w, httptest.NewRequest(tc.method, "/", nil))
			if w.Code != tc.status || w.calls != tc.calls || event.Status != tc.status {
				t.Fatalf("status=%d calls=%d event=%+v", w.Code, w.calls, event)
			}
			if tc.status == 201 {
				want := text
				if tc.method == "HEAD" {
					want = ""
				}
				if w.Body.String() != want || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
					t.Fatalf("body=%q header=%v", w.Body.String(), w.Header())
				}
			}
		})
	}
}

func TestPlainTextStringWriterFailureRetainsDeliveryOutcome(t *testing.T) {
	var event trest.Event
	app, err := trest.New(trest.AfterResponse(func(e trest.Event) { event = e }), trest.Get("/", trest.Const("text")))
	if err != nil {
		t.Fatal(err)
	}
	w := &stringResponse{ResponseRecorder: httptest.NewRecorder(), err: errors.New("transport failure")}
	recovered := protocolPanic(func() { app.ServeHTTP(w, httptest.NewRequest("GET", "/", nil)) })
	if recovered != http.ErrAbortHandler || event.Code != operation.Internal || event.Status != 200 || w.Body.Len() != 0 {
		t.Fatalf("panic=%v event=%+v body=%q", recovered, event, w.Body.String())
	}
}
