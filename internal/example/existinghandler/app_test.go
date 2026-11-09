package existinghandler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trest.local/trest/internal/example/existinghandler"
)

func TestExistingHandlersCoexist(t *testing.T) {
	t.Parallel()
	app, err := existinghandler.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{"existing health handler", "/healthz", "ok\n"},
		{"typed public operation", "/hello", `"message":"Hello from Trest"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			app.Handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tc.body) {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
		})
	}
	data, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Paths) != 1 || document.Paths["/hello"] == nil {
		t.Fatalf("contract should contain only explicitly bound operations: %s", data)
	}
}
