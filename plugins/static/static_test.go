package static_test

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"trest.local/trest"
	staticfiles "trest.local/trest/plugins/static"
)

func TestStaticFilesAndTraversal(t *testing.T) {
	files := fstest.MapFS{"hello.txt": {Data: []byte("hello")}, "nested/style.css": {Data: []byte("body{}")}, ".env": {Data: []byte("secret")}, "large.txt": {Data: []byte(strings.Repeat("x", 100))}}
	plugin, err := staticfiles.New("/assets", files, staticfiles.Options{MaxBytes: 50, CacheControl: "public, max-age=60"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := trest.New(plugin)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		status     int
	}{
		{"file", "/assets/hello.txt", 200}, {"nested", "/assets/nested/style.css", 200}, {"directory", "/assets/nested/", 404}, {"hidden", "/assets/.env", 404}, {"encoded traversal", "/assets/%2e%2e/.env", 404}, {"encoded slash", "/assets/nested%2f..%2f.env", 404}, {"large", "/assets/large.txt", 413}, {"missing", "/assets/missing", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.status == 200 && w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing nosniff")
			}
		})
	}
}
func TestStaticRangeAndHead(t *testing.T) {
	plugin, err := staticfiles.New("/assets", fstest.MapFS{"hello.txt": {Data: []byte("hello")}}, staticfiles.Options{})
	if err != nil {
		t.Fatal(err)
	}
	app, err := trest.New(plugin)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/assets/hello.txt", nil)
	r.Header.Set("Range", "bytes=1-3")
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "ell" {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("HEAD", "/assets/hello.txt", nil))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("head: %d %q", w.Code, w.Body.String())
	}
}
