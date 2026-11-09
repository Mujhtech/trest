package webinput_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"trest.local/trest/internal/webinput"
)

func TestBodylessRequestChecksTrustReaderNotContentLength(t *testing.T) {
	plan, err := webinput.Compile(reflect.TypeFor[struct{}](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, query string
		reader            int
		length            int64
		status            int
	}{
		{name: "nil body"},
		{name: "canonical empty", reader: 1},
		{name: "other empty reader", reader: 2},
		{name: "body despite zero length", reader: 2, body: "x", status: 400},
		{name: "body with unknown length", reader: 2, body: "x", length: -1, status: 400},
		{name: "oversized declared canonical empty", reader: 1, length: 65, status: 413},
		{name: "malformed query", reader: 1, query: "x=%zz", status: 400},
		{name: "unknown query", reader: 1, query: "extra=1", status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.Body = nil
			switch tc.reader {
			case 1:
				req.Body = http.NoBody
			case 2:
				req.Body = io.NopCloser(strings.NewReader(tc.body))
			}
			req.ContentLength = tc.length
			req.URL.RawQuery = tc.query
			_, cleanup, err := plan.Decode(httptest.NewRecorder(), req, 64)
			if cleanup != nil {
				defer cleanup()
			}
			if tc.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var input *webinput.Error
				if !errors.As(err, &input) || input.Status != tc.status {
					t.Fatalf("err=%v; want status %d", err, tc.status)
				}
			}
		})
	}
}
