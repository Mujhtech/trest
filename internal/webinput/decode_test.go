package webinput_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mujhtech/trest/internal/webinput"
	"github.com/mujhtech/trest/operation"
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

func TestDecodeIntoMatchesFreshDecode(t *testing.T) {
	t.Parallel()
	type input struct {
		ID      int           `path:"id" min:"1"`
		Limit   int           `query:"limit" default:"10" min:"1" max:"20"`
		Price   *codecDecimal `query:"price"`
		Token   *string       `header:"X-Token"`
		Ignored string        `json:"-"`
		Body    struct {
			Name string `json:"name" minLength:"2"`
		} `body:"json"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "POST", "/items/{id}")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query, body, media string
		max                      int64
		status                   int
	}{
		{name: "exact media type", body: `{"name":"Ada"}`, media: "application/json", max: 128},
		{name: "MIME case and whitespace", body: `{"name":"Ada"}`, media: "Application/JSON ; Charset=\"UTF-8\"", max: 128},
		{name: "explicit fields and codec", query: "limit=5&price=12.50", body: `{"name":"Ada"}`, media: "application/json", max: 128},
		{name: "wrong charset", body: `{"name":"Ada"}`, media: "application/json;charset=utf-16", max: 128, status: 415},
		{name: "malformed MIME parameter", body: `{"name":"Ada"}`, media: "application/json;charset", max: 128, status: 415},
		{name: "duplicate MIME parameter", body: `{"name":"Ada"}`, media: "application/json;charset=utf-8;charset=utf-16", max: 128, status: 415},
		{name: "malformed query", query: "limit=%zz", body: `{"name":"Ada"}`, media: "application/json", max: 128, status: 400},
		{name: "unknown query", query: "tenant=other", body: `{"name":"Ada"}`, media: "application/json", max: 128, status: 400},
		{name: "duplicate query", query: "limit=5&limit=6", body: `{"name":"Ada"}`, media: "application/json", max: 128, status: 400},
		{name: "codec error", query: "price=13.37", body: `{"name":"Ada"}`, media: "application/json", max: 128, status: 400},
		{name: "codec panic", query: "price=66.66", body: `{"name":"Ada"}`, media: "application/json", max: 128, status: 400},
		{name: "wire constraint", body: `{"name":"A"}`, media: "application/json", max: 128, status: 400},
		{name: "duplicate body field", body: `{"name":"Ada","name":"Eve"}`, media: "application/json", max: 128, status: 400},
		{name: "unknown body field", body: `{"name":"Ada","admin":true}`, media: "application/json", max: 128, status: 400},
		{name: "trailing value", body: `{"name":"Ada"} {}`, media: "application/json", max: 128, status: 400},
		{name: "bounded reader", body: `{"name":"Ada"}`, media: "application/json", max: 8, status: 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := func() *http.Request {
				r := httptest.NewRequest("POST", "/items/1?"+tc.query, strings.NewReader(tc.body))
				r.SetPathValue("id", "1")
				r.Header.Set("Content-Type", tc.media)
				// A false declared length must not bypass bounded body reads.
				r.ContentLength = 0
				return r
			}
			want, cleanup, wantErr := plan.Decode(httptest.NewRecorder(), request(), tc.max)
			defer cleanup()
			stale := "must not survive reuse"
			got := input{ID: 99, Limit: 99, Token: &stale, Ignored: stale}
			cleanup, gotErr := plan.DecodeInto(httptest.NewRecorder(), request(), tc.max, &got)
			defer cleanup()
			if tc.status == 0 {
				if wantErr != nil || gotErr != nil {
					t.Fatalf("Decode=%v, DecodeInto=%v", wantErr, gotErr)
				}
				if !reflect.DeepEqual(got, want.Interface()) {
					t.Fatalf("DecodeInto=%+v, Decode=%+v", got, want.Interface())
				}
				return
			}
			var wantBinding, gotBinding *webinput.Error
			if !errors.As(wantErr, &wantBinding) || !errors.As(gotErr, &gotBinding) || wantBinding.Status != tc.status || gotBinding.Status != tc.status {
				t.Fatalf("Decode=%v, DecodeInto=%v; want status %d", wantErr, gotErr, tc.status)
			}
			wantPublic, gotPublic := operation.PublicError(wantErr), operation.PublicError(gotErr)
			if wantPublic.Code != gotPublic.Code || wantPublic.Message != gotPublic.Message || !reflect.DeepEqual(wantPublic.Fields, gotPublic.Fields) {
				t.Fatalf("public errors differ: Decode=%+v, DecodeInto=%+v", wantPublic, gotPublic)
			}
		})
	}
}

func TestDecodeIntoRejectsWrongDestination(t *testing.T) {
	t.Parallel()
	type input struct{ Name string }
	plan, err := webinput.Compile(reflect.TypeFor[input](), "POST", "/")
	if err != nil {
		t.Fatal(err)
	}
	var absent *input
	for _, tc := range []struct {
		name   string
		target any
	}{
		{name: "nil"},
		{name: "typed nil", target: absent},
		{name: "not a pointer", target: input{}},
		{name: "wrong type", target: new(string)},
		{name: "pointer to pointer", target: &absent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", strings.NewReader(`{"Name":"Ada"}`))
			r.Header.Set("Content-Type", "application/json")
			cleanup, err := plan.DecodeInto(httptest.NewRecorder(), r, 128, tc.target)
			defer cleanup()
			var bindingErr *webinput.Error
			if !errors.As(err, &bindingErr) || bindingErr.Status != 500 || operation.CodeOf(err) != operation.Internal {
				t.Fatalf("unexpected configuration error: %v", err)
			}
		})
	}
}
