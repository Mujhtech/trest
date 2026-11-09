package trest_test

import (
	"errors"
	"strings"
	"testing"

	"trest.local/trest"
)

func TestConstructionErrorsLocateEveryDeclaration(t *testing.T) {
	t.Parallel()
	_, err := trest.New(trest.Get("/local", trest.Const("hello")).Name("not a name"), invalidDeclaredElsewhere())
	if err == nil {
		t.Fatal("invalid declarations accepted")
	}
	var diagnostic *trest.ConstructionError
	if !errors.As(err, &diagnostic) || diagnostic.Source.Line <= 0 {
		t.Fatalf("missing structured location: %v", err)
	}
	for _, part := range []string{"GET /local", "diagnostics_test.go:", "POST /other", "diagnostics_fixture_test.go:"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("missing %q in %v", part, err)
		}
	}
}
