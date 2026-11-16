package webinput_test

import (
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/mujhtech/trest/internal/webinput"
	"github.com/mujhtech/trest/operation"
)

type readOnlyNativeInput struct {
	Name  string `json:"name" minLength:"2"`
	Count int    `json:"count" min:"1"`
}

type trackedRequestBody struct {
	reader *strings.Reader
	read   int
	closed bool
}

func (b *trackedRequestBody) Read(dst []byte) (int, error) {
	n, err := b.reader.Read(dst)
	b.read += n
	return n, err
}

func (b *trackedRequestBody) Close() error {
	b.closed = true
	return nil
}

func TestDecodeIntoReadOnlyMatchesDecodeInto(t *testing.T) {
	t.Parallel()
	type codecInput struct {
		Amount codecDecimal `json:"amount"`
	}
	type explicitInput struct {
		ID    int                 `path:"id" min:"1"`
		Limit int                 `query:"limit" default:"10" min:"1" max:"20"`
		Price *codecDecimal       `query:"price"`
		Alias []string            `header:"X-Alias"`
		Token *string             `cookie:"session"`
		Body  readOnlyNativeInput `body:"json"`
	}
	type bodylessInput struct {
		Ignored string `json:"-"`
	}
	for _, test := range []struct {
		name, method, path, target, body, contentType string
		typ                                           reflect.Type
		reader                                        int // 0: ordinary reader, 1: nil, 2: NoBody
		max, length                                   int64
		status                                        int
	}{
		{name: "native JSON", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":"Ada","count":2}`, max: 128},
		{name: "native Unicode", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":"\ud800\ud800","count":2}`, max: 128},
		{name: "custom codec", typ: reflect.TypeFor[codecInput](), body: `{"amount":"12.50"}`, max: 128},
		{name: "custom codec error", typ: reflect.TypeFor[codecInput](), body: `{"amount":"13.37"}`, max: 128, status: 400},
		{name: "custom codec panic", typ: reflect.TypeFor[codecInput](), body: `{"amount":"66.66"}`, max: 128, status: 400},
		{name: "custom codec wire rejection", typ: reflect.TypeFor[codecInput](), body: `{"amount":"invalid"}`, max: 128, status: 400},
		{name: "explicit sources", typ: reflect.TypeFor[explicitInput](), path: "/items/{id}", target: "/items/2?limit=4&price=12.50", body: `{"name":"Ada","count":2}`, max: 128},
		{name: "explicit source failure", typ: reflect.TypeFor[explicitInput](), path: "/items/{id}", target: "/items/2?limit=99", body: `{"name":"Ada","count":2}`, max: 128, status: 400},
		{name: "bodyless nil", method: "GET", typ: reflect.TypeFor[bodylessInput](), reader: 1, max: 128},
		{name: "bodyless NoBody", method: "GET", typ: reflect.TypeFor[bodylessInput](), reader: 2, max: 128},
		{name: "bodyless empty reader", method: "GET", typ: reflect.TypeFor[bodylessInput](), max: 128},
		{name: "unexpected body with false length", method: "GET", typ: reflect.TypeFor[bodylessInput](), body: "x", max: 128, status: 400},
		{name: "bodyless excessive declared length", method: "GET", typ: reflect.TypeFor[bodylessInput](), reader: 2, max: 128, length: 129, status: 413},
		{name: "unknown query", typ: reflect.TypeFor[readOnlyNativeInput](), target: "/?unknown=1", body: `{"name":"Ada","count":2}`, max: 128, status: 400},
		{name: "malformed query", typ: reflect.TypeFor[readOnlyNativeInput](), target: "/?bad=%zz", body: `{"name":"Ada","count":2}`, max: 128, status: 400},
		{name: "duplicate body field", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":"Ada","name":"Eve","count":2}`, max: 128, status: 400},
		{name: "unknown body field", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":"Ada","count":2,"admin":true}`, max: 128, status: 400},
		{name: "missing required body field", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":"Ada"}`, max: 128, status: 400},
		{name: "wire constraint", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":"A","count":2}`, max: 128, status: 400},
		{name: "trailing JSON", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":"Ada","count":2} {}`, max: 128, status: 400},
		{name: "invalid JSON", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":`, max: 128, status: 400},
		{name: "nil JSON body", typ: reflect.TypeFor[readOnlyNativeInput](), reader: 1, max: 128, status: 400},
		{name: "NoBody JSON body", typ: reflect.TypeFor[readOnlyNativeInput](), reader: 2, max: 128, status: 400},
		{name: "wrong media type", typ: reflect.TypeFor[readOnlyNativeInput](), contentType: "text/plain", body: `{"name":"Ada","count":2}`, max: 128, status: 415},
		{name: "bounded native body with false length", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":"Ada","count":2}`, max: 8, status: 413},
		{name: "bounded custom body with false length", typ: reflect.TypeFor[codecInput](), body: `{"amount":"12.50"}`, max: 8, status: 413},
		{name: "excessive declared length", typ: reflect.TypeFor[readOnlyNativeInput](), body: `{"name":"Ada","count":2}`, max: 128, length: 129, status: 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			method, path, target := test.method, test.path, test.target
			if method == "" {
				method = http.MethodPost
			}
			if path == "" {
				path = "/"
			}
			if target == "" {
				target = "/"
			}
			plan, err := webinput.Compile(test.typ, method, path)
			if err != nil {
				t.Fatal(err)
			}
			if !plan.CanDecodeReadOnly() {
				t.Fatal("nonform plan rejected read-only decoding")
			}
			request := func() (*http.Request, *trackedRequestBody) {
				type contextKey struct{}
				r := httptest.NewRequest(method, target, nil).WithContext(context.WithValue(context.Background(), contextKey{}, "preserved"))
				body := &trackedRequestBody{reader: strings.NewReader(test.body)}
				switch test.reader {
				case 0:
					r.Body = body
				case 1:
					r.Body = nil
				case 2:
					r.Body = http.NoBody
				}
				r.ContentLength = test.length
				contentType := test.contentType
				if contentType == "" {
					contentType = "application/json"
				}
				r.Header.Set("Content-Type", contentType)
				r.Header.Set("X-Alias", "first, second")
				r.Header.Set("Cookie", "session=token")
				r.SetPathValue("id", "2")
				r.Form = url.Values{"preserve-form": {"form"}}
				r.PostForm = url.Values{"preserve-post": {"post"}}
				r.MultipartForm = &multipart.Form{Value: map[string][]string{"preserve-multipart": {"multipart"}}}
				return r, body
			}
			regularRequest, regularBody := request()
			want := reflect.New(test.typ)
			cleanup, wantErr := plan.DecodeInto(httptest.NewRecorder(), regularRequest, test.max, want.Interface())
			defer cleanup()

			r, body := request()
			snapshot := r.Clone(r.Context())
			originalMultipart := r.MultipartForm
			got := reflect.New(test.typ)
			if ignored := got.Elem().FieldByName("Ignored"); ignored.IsValid() {
				ignored.SetString("must be cleared")
			}
			cleanup, gotErr := plan.DecodeIntoReadOnly(httptest.NewRecorder(), r, test.max, got.Interface())
			cleanup()
			if r.Body != snapshot.Body || r.Context() != snapshot.Context() || r.MultipartForm != originalMultipart ||
				!reflect.DeepEqual(r.Form, snapshot.Form) || !reflect.DeepEqual(r.PostForm, snapshot.PostForm) ||
				!reflect.DeepEqual(r.MultipartForm, snapshot.MultipartForm) || !reflect.DeepEqual(r.Header, snapshot.Header) ||
				!reflect.DeepEqual(r.URL, snapshot.URL) || r.ContentLength != snapshot.ContentLength || r.PathValue("id") != "2" {
				t.Fatal("read-only binding changed request fields")
			}
			if body.read != regularBody.read || body.closed || regularBody.closed {
				t.Fatalf("body consumption differs: read-only=%d ordinary=%d closed=%v/%v", body.read, regularBody.read, body.closed, regularBody.closed)
			}
			if test.status == 0 {
				if wantErr != nil || gotErr != nil {
					t.Fatalf("ordinary=%v read-only=%v", wantErr, gotErr)
				}
				if !reflect.DeepEqual(got.Elem().Interface(), want.Elem().Interface()) {
					t.Fatalf("ordinary=%+v read-only=%+v", want.Elem().Interface(), got.Elem().Interface())
				}
				return
			}
			var wantBinding, gotBinding *webinput.Error
			if !errors.As(wantErr, &wantBinding) || !errors.As(gotErr, &gotBinding) || wantBinding.Status != test.status || gotBinding.Status != test.status {
				t.Fatalf("ordinary=%v read-only=%v, want status %d", wantErr, gotErr, test.status)
			}
			wantPublic, gotPublic := operation.PublicError(wantErr), operation.PublicError(gotErr)
			if wantPublic.Code != gotPublic.Code || wantPublic.Message != gotPublic.Message || !reflect.DeepEqual(wantPublic.Fields, gotPublic.Fields) {
				t.Fatalf("public errors differ: ordinary=%+v read-only=%+v", wantPublic, gotPublic)
			}
		})
	}
}

func TestDecodeIntoReadOnlyRejectsFormsBeforeConsuming(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		typ  reflect.Type
	}{
		{"form", reflect.TypeFor[struct {
			Name string `form:"name"`
		}]()},
		{"multipart", reflect.TypeFor[struct {
			File *multipart.FileHeader `form:"file" json:"-"`
		}]()},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := webinput.Compile(test.typ, "POST", "/")
			if err != nil {
				t.Fatal(err)
			}
			if plan.CanDecodeReadOnly() {
				t.Fatal("form plan accepted read-only decoding")
			}
			body := &trackedRequestBody{reader: strings.NewReader("name=Ada")}
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.Body = body
			cleanup, err := plan.DecodeIntoReadOnly(httptest.NewRecorder(), r, 128, reflect.New(test.typ).Interface())
			cleanup()
			var bindingErr *webinput.Error
			if !errors.As(err, &bindingErr) || bindingErr.Status != 500 || operation.CodeOf(err) != operation.Internal {
				t.Fatalf("unexpected configuration error: %v", err)
			}
			if r.Body != body || body.read != 0 || body.closed || r.Form != nil || r.PostForm != nil || r.MultipartForm != nil {
				t.Fatal("rejected read-only form binding consumed or changed the request")
			}
		})
	}
}

func TestDecodeIntoReadOnlyRejectsWrongDestination(t *testing.T) {
	t.Parallel()
	var nilPlan *webinput.Plan
	if nilPlan.CanDecodeReadOnly() {
		t.Fatal("nil plan accepted read-only decoding")
	}
	plan, err := webinput.Compile(reflect.TypeFor[readOnlyNativeInput](), "POST", "/")
	if err != nil {
		t.Fatal(err)
	}
	var absent *readOnlyNativeInput
	for _, test := range []struct {
		name   string
		target any
	}{
		{name: "nil"},
		{name: "typed nil", target: absent},
		{name: "not pointer", target: readOnlyNativeInput{}},
		{name: "wrong type", target: new(string)},
		{name: "pointer to pointer", target: &absent},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"Ada","count":2}`))
			originalBody := r.Body
			cleanup, err := plan.DecodeIntoReadOnly(httptest.NewRecorder(), r, 128, test.target)
			cleanup()
			var bindingErr *webinput.Error
			if !errors.As(err, &bindingErr) || bindingErr.Status != 500 || operation.CodeOf(err) != operation.Internal || r.Body != originalBody {
				t.Fatalf("unexpected configuration result: %v", err)
			}
		})
	}
}
