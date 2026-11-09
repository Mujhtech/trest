package cors_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trest.local/trest"
	"trest.local/trest/operation"
	"trest.local/trest/plugins/cors"
)

func TestCORSRequests(t *testing.T) {
	middleware, err := cors.Handler(cors.Options{Origins: []string{"https://app.example"}, Methods: []string{"GET", "POST"}, Headers: []string{"Content-Type", "Authorization"}, ExposeHeaders: []string{"X-Request-ID"}, Credentials: true, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, origin, requestMethod, requestHeaders string
		want                                                int
		calls                                               int
	}{
		{"allowed actual", "GET", "https://app.example", "", "", 200, 1},
		{"nonbrowser", "GET", "", "", "", 200, 1},
		{"denied origin", "GET", "https://evil.example", "", "", 403, 0},
		{"origin suffix", "GET", "https://app.example.evil.test", "", "", 403, 0},
		{"opaque origin", "GET", "null", "", "", 403, 0},
		{"preflight", "OPTIONS", "https://app.example", "POST", "content-type, authorization", 204, 0},
		{"denied method", "OPTIONS", "https://app.example", "DELETE", "", 403, 0},
		{"denied header", "OPTIONS", "https://app.example", "POST", "X-Admin", 403, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(200) }))
			request := httptest.NewRequest(tc.method, "http://api.example/invoices", nil)
			if tc.origin != "" {
				request.Header.Set("Origin", tc.origin)
			}
			if tc.requestMethod != "" {
				request.Header.Set("Access-Control-Request-Method", tc.requestMethod)
			}
			if tc.requestHeaders != "" {
				request.Header.Set("Access-Control-Request-Headers", tc.requestHeaders)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want || calls != tc.calls {
				t.Fatalf("status=%d calls=%d", response.Code, calls)
			}
			if tc.want == 403 && response.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("denied response granted CORS")
			}
			if tc.origin != "" && tc.want != 403 && response.Header().Get("Access-Control-Allow-Origin") != tc.origin {
				t.Fatal("origin missing")
			}
			if tc.want == 204 && response.Header().Get("Access-Control-Allow-Credentials") != "true" {
				t.Fatal("credentials absent")
			}
			if !strings.Contains(strings.Join(response.Header().Values("Vary"), ","), "Origin") {
				t.Fatal("missing Vary")
			}
		})
	}
}
func TestCORSRejectsUnsafeConfiguration(t *testing.T) {
	for _, options := range []cors.Options{{}, {Origins: []string{"*"}, Credentials: true}, {Origins: []string{"https://app.example/path"}}, {Origins: []string{"https://app.example"}, Methods: []string{"GET\r\nX-Header"}}} {
		if _, err := cors.New(options); err == nil {
			t.Fatal("accepted unsafe config")
		}
	}
}
func TestCORSCopiesPolicyAndRejectsDuplicateOrigin(t *testing.T) {
	origins := []string{"https://app.example"}
	middleware, err := cors.Handler(cors.Options{Origins: origins})
	if err != nil {
		t.Fatal(err)
	}
	origins[0] = "https://evil.example"
	next := middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Add("Origin", "https://app.example")
	r.Header.Add("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	next.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestCORSPreflightBypassesAuthButActualRequestDoesNot(t *testing.T) {
	component, err := cors.New(cors.Options{Origins: []string{"https://app.example"}, Methods: []string{"POST"}, Headers: []string{"Content-Type"}})
	if err != nil {
		t.Fatal(err)
	}
	authCalls, handlerCalls := 0, 0
	app, err := trest.New(component,
		trest.Authenticate(func(*http.Request) (trest.Principal, error) { authCalls++; return trest.Principal{}, nil }),
		trest.Authorization(operation.AuthorizerFunc(func(context.Context, operation.Principal, string, any) error { return nil })),
		trest.Raw("POST", "/invoices", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { handlerCalls++; w.WriteHeader(201) })).Permission("invoices:write"),
	)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("OPTIONS", "/invoices", nil)
	request.Header.Set("Origin", "https://app.example")
	request.Header.Set("Access-Control-Request-Method", "POST")
	request.Header.Set("Access-Control-Request-Headers", "Content-Type")
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != 204 || authCalls != 0 || handlerCalls != 0 {
		t.Fatalf("preflight=%d auth=%d handler=%d", response.Code, authCalls, handlerCalls)
	}
	request = httptest.NewRequest("POST", "/invoices", strings.NewReader("{}"))
	request.Header.Set("Origin", "https://app.example")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	app.ServeHTTP(response, request)
	if response.Code != 401 || authCalls != 1 || handlerCalls != 0 {
		t.Fatalf("actual=%d auth=%d handler=%d", response.Code, authCalls, handlerCalls)
	}
	if response.Header().Get("Access-Control-Allow-Origin") != "https://app.example" {
		t.Fatal("browser cannot read auth error")
	}
}
