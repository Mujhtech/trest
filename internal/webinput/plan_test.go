package webinput_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mujhtech/trest/internal/webinput"
	"github.com/mujhtech/trest/operation"
)

func TestCompileMetadataAndDecodeSources(t *testing.T) {
	t.Parallel()
	type body struct {
		Name string `json:"name" minLength:"2"`
	}
	type input struct {
		ID      string   `path:"id"`
		Limit   int      `query:"limit" default:"20" min:"1" max:"50"`
		Tags    []string `query:"tag" required:"false"`
		Secret  *string  `header:"X-Secret"`
		Session *string  `cookie:"session"`
		Body    body     `body:"json"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "POST", "/users/{id}")
	if err != nil {
		t.Fatal(err)
	}
	if plan.BodyContentType != "application/json" || !plan.BodyRequired || plan.NativeOnly || len(plan.Parameters) != 5 {
		t.Fatalf("plan=%+v", plan)
	}
	var spec map[string]any
	if err := json.Unmarshal(plan.BodySchema, &spec); err != nil {
		t.Fatal(err)
	}
	if spec["properties"].(map[string]any)["name"].(map[string]any)["minLength"] != float64(2) {
		t.Fatal("body constraints absent")
	}
	r := httptest.NewRequest("POST", "/users/123?tag=one&tag=two", strings.NewReader(`{"name":"Ada"}`))
	r.SetPathValue("id", "123")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Secret", "header-secret")
	r.AddCookie(&http.Cookie{Name: "session", Value: "cookie-secret"})
	v, cleanup, err := plan.Decode(httptest.NewRecorder(), r, 4096)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	got := v.Interface().(input)
	if got.ID != "123" || got.Limit != 20 || !reflect.DeepEqual(got.Tags, []string{"one", "two"}) || got.Secret == nil || *got.Secret != "header-secret" || got.Session == nil || *got.Session != "cookie-secret" || got.Body.Name != "Ada" {
		t.Fatalf("decoded=%+v", got)
	}
	// Public metadata can be annotated by callers without changing execution.
	plan.Parameters[0].Name = "changed"
}

func TestHeaderArraysUseSimpleLists(t *testing.T) {
	t.Parallel()
	type input struct {
		Labels []string `header:"X-Label"`
		Counts []int    `header:"X-Count"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "GET", "/headers")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/headers", nil)
	r.Header.Add("X-Label", " one,\ttwo ")
	r.Header.Add("X-Label", "three")
	r.Header.Add("X-Count", "1, 2")
	r.Header.Add("X-Count", "\t3\t")
	value, cleanup, err := plan.Decode(httptest.NewRecorder(), r, 1024)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	got := value.Interface().(input)
	if !reflect.DeepEqual(got.Labels, []string{"one", "two", "three"}) || !reflect.DeepEqual(got.Counts, []int{1, 2, 3}) {
		t.Fatalf("header values = %#v", got)
	}
}

func TestDecodeRejectsInvalidBeforeReturningInput(t *testing.T) {
	t.Parallel()
	type input struct {
		ID      int     `path:"id" min:"1"`
		Limit   int     `query:"limit" default:"10" min:"1" max:"20"`
		Session *string `cookie:"session"`
		Body    struct {
			Name string `json:"name"`
		} `body:"json"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "POST", "/users/{id}")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query, path, body, media, cookie string
		max                                    int64
		status                                 int
	}{
		{"duplicate query", "limit=2&limit=3", "1", `{"name":"Ada"}`, "application/json", "", 4096, 400},
		{"bad number", "limit=1.5", "1", `{"name":"Ada"}`, "application/json", "", 4096, 400},
		{"range", "limit=99", "1", `{"name":"Ada"}`, "application/json", "", 4096, 400},
		{"unknown query", "tenant=evil", "1", `{"name":"Ada"}`, "application/json", "", 4096, 400},
		{"query cannot replace path", "id=99", "1", `{"name":"Ada"}`, "application/json", "", 4096, 400},
		{"invalid query", "limit=%XX", "1", `{"name":"Ada"}`, "application/json", "", 4096, 400},
		{"path range", "", "0", `{"name":"Ada"}`, "application/json", "", 4096, 400},
		{"path text", "", "not-an-integer", `{"name":"Ada"}`, "application/json", "", 4096, 400},
		{"duplicate cookie", "", "1", `{"name":"Ada"}`, "application/json", "session=one; session=two", 4096, 400},
		{"duplicate body", "", "1", `{"name":"Ada","name":"other"}`, "application/json", "", 4096, 400},
		{"unknown body", "", "1", `{"name":"Ada","tenant":"evil"}`, "application/json", "", 4096, 400},
		{"wrong media", "", "1", `{"name":"Ada"}`, "text/plain", "", 4096, 415},
		{"wrong charset", "", "1", `{"name":"Ada"}`, "application/json;charset=utf-16", "", 4096, 415},
		{"too large", "", "1", `{"name":"Ada"}`, "application/json", "", 8, 413},
		{"empty body", "", "1", ``, "application/json", "", 4096, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/users/1?"+tc.query, strings.NewReader(tc.body))
			r.SetPathValue("id", tc.path)
			r.Header.Set("Content-Type", tc.media)
			if tc.cookie != "" {
				r.Header.Set("Cookie", tc.cookie)
			}
			v, cleanup, err := plan.Decode(httptest.NewRecorder(), r, tc.max)
			defer cleanup()
			var bindingError *webinput.Error
			if !errors.As(err, &bindingError) || bindingError.Status != tc.status || v.IsValid() {
				t.Fatalf("value=%v error=%v", v, err)
			}
			if operation.CodeOf(err) != operation.InvalidInput {
				t.Fatalf("classification lost: %v", err)
			}
		})
	}
}

func TestWholeBodyAndBodylessRequests(t *testing.T) {
	t.Parallel()
	type body struct{ Name string }
	plan, err := webinput.Compile(reflect.TypeFor[body](), "POST", "/users")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/users", strings.NewReader(`{"Name":"Ada"}`))
	r.Header.Set("Content-Type", "application/json; charset=UTF-8")
	v, cleanup, err := plan.Decode(httptest.NewRecorder(), r, 4096)
	defer cleanup()
	if err != nil || v.Interface().(body).Name != "Ada" {
		t.Fatalf("%v %v", v, err)
	}
	empty, err := webinput.Compile(reflect.TypeFor[struct{}](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	_, cleanup, err = empty.Decode(httptest.NewRecorder(), httptest.NewRequest("GET", "/", strings.NewReader("surprise")), 4096)
	defer cleanup()
	if err == nil {
		t.Fatal("bodyless route accepted a body")
	}
}

func TestDecodeURLFormDoesNotMergeQuery(t *testing.T) {
	t.Parallel()
	type input struct {
		Name  string  `form:"name" minLength:"2"`
		Count int     `form:"count" min:"1" max:"5"`
		Trace *string `query:"trace"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "POST", "/form")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/form?trace=query", strings.NewReader("name=Ada&count=3"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	v, cleanup, err := plan.Decode(httptest.NewRecorder(), r, 4096)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	got := v.Interface().(input)
	if got.Name != "Ada" || got.Count != 3 || got.Trace == nil || *got.Trace != "query" {
		t.Fatalf("decoded=%+v", got)
	}
	for _, body := range []string{"name=Ada&count=3&tenant=evil", "name=Ada&count=3&count=4", "name=Ada&count=9"} {
		r := httptest.NewRequest("POST", "/form", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		_, cleanup, err := plan.Decode(httptest.NewRecorder(), r, 4096)
		cleanup()
		if err == nil {
			t.Fatalf("invalid form accepted: %s", body)
		}
	}
}

func TestMultipartUploadsStayUsableUntilCleanup(t *testing.T) {
	t.Parallel()
	type input struct {
		Name string                `form:"name"`
		File *multipart.FileHeader `form:"file" json:"-" required:"true"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "POST", "/upload")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.NativeOnly || plan.BodyContentType != "multipart/form-data" {
		t.Fatal("file route was not marked native")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("name", "receipt"); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("file", "receipt.txt")
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", 100<<10)
	if _, err := io.WriteString(file, content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/upload", bytes.NewReader(body.Bytes()))
	r.Header.Set("Content-Type", writer.FormDataContentType())
	v, cleanup, err := plan.Decode(httptest.NewRecorder(), r, 1<<20)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	got := v.Interface().(input)
	if got.Name != "receipt" || got.File.Filename != "receipt.txt" {
		t.Fatalf("decoded=%+v", got)
	}
	opened, err := got.File.Open()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(opened)
	if closeErr := opened.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || string(data) != content {
		t.Fatal("uploaded content changed")
	}
	cleanup()
	if opened, err := got.File.Open(); err == nil {
		_ = opened.Close()
		t.Fatal("disk-backed upload survived cleanup")
	}
	// Unknown-length request bodies are bounded while multipart parsing reads.
	r = httptest.NewRequest("POST", "/upload", bytes.NewReader(body.Bytes()))
	r.ContentLength = -1
	r.Header.Set("Content-Type", writer.FormDataContentType())
	_, cleanup, err = plan.Decode(httptest.NewRecorder(), r, 100)
	defer cleanup()
	var bindingError *webinput.Error
	if !errors.As(err, &bindingError) || bindingError.Status != 413 {
		t.Fatalf("oversized upload=%v", err)
	}
}

func TestCompileRejectsAmbiguousDeclarations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		typ          reflect.Type
		method, path string
	}{
		{"multiple sources", reflect.TypeFor[struct {
			ID string `path:"id" query:"id"`
		}](), "GET", "/{id}"},
		{"missing path", reflect.TypeFor[struct {
			ID string `path:"id"`
		}](), "GET", "/"},
		{"missing field", reflect.TypeFor[struct{}](), "GET", "/{id}"},
		{"implicit query", reflect.TypeFor[struct{ ID string }](), "GET", "/"},
		{"duplicate query", reflect.TypeFor[struct {
			A string `query:"id"`
			B string `query:"id"`
		}](), "GET", "/"},
		{"mixed body forms", reflect.TypeFor[struct {
			A    string    `form:"a"`
			Body bodyInput `body:"json"`
		}](), "POST", "/"},
		{"body source", reflect.TypeFor[struct {
			Body string `body:"text"`
		}](), "POST", "/"},
		{"upload hidden", reflect.TypeFor[struct {
			File *multipart.FileHeader `form:"file"`
		}](), "POST", "/"},
		{"parameter hidden", reflect.TypeFor[struct {
			ID string `query:"id" json:"-"`
		}](), "GET", "/"},
		{"wrong type", reflect.TypeFor[struct {
			Value map[string]string `query:"value"`
		}](), "GET", "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := webinput.Compile(tc.typ, tc.method, tc.path); err == nil {
				t.Fatal("ambiguous request declaration accepted")
			}
		})
	}
}

type bodyInput struct{ Name string }

func TestTypedOptionalScalars(t *testing.T) {
	t.Parallel()
	type input struct {
		At     *time.Time `query:"at"`
		Active bool       `query:"active" default:"false"`
		Amount float64    `query:"amount" default:"1"`
		ID     uint8      `query:"id" default:"1"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query string
		valid       bool
	}{
		{"defaults", "", true}, {"timestamp", "at=2026-11-10T08%3A00%3A00Z", true}, {"invalid timestamp", "at=tomorrow", false}, {"strict bool", "active=1", false}, {"overflow", "id=256", false}, {"negative unsigned", "id=-1", false}, {"nonfinite", "amount=NaN", false}, {"padded integer", "id=01", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, cleanup, err := plan.Decode(httptest.NewRecorder(), httptest.NewRequest("GET", "/?"+tc.query, nil), 4096)
			defer cleanup()
			if (err == nil) != tc.valid {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
