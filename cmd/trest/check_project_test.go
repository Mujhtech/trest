package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mujhtech/trest"
)

func TestProjectBaselineIgnoresDisplayMetadataAndPreservesSchema(t *testing.T) {
	t.Parallel()
	first := []byte(`{"openapi":"3.1.1","info":{"title":"First","version":"1.0"},"paths":{},"components":{"schemas":{"ID":{"type":"integer","maximum":18446744073709551615}}}}`)
	second := []byte(`{ "components": {"schemas":{"ID":{"maximum":18446744073709551615,"type":"integer"}}}, "paths":{}, "info":{"version":"2.0","title":"Second"}, "openapi":"3.1.1" }`)
	a, err := normalizeProjectContract(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := normalizeProjectContract(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) || !bytes.Contains(a, []byte("18446744073709551615")) {
		t.Fatalf("baseline changed display metadata or integer precision:\n%s\n%s", a, b)
	}
	changed, err := normalizeProjectContract(bytes.Replace(second, []byte("18446744073709551615"), []byte("18446744073709551614"), 1))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, changed) {
		t.Fatal("schema change disappeared from baseline")
	}
}

func TestProjectCheckRejectsInconsistentExports(t *testing.T) {
	t.Parallel()
	app, err := trest.New(
		trest.Get("/alpha", trest.Const("alpha")).Name("alpha"),
		trest.Raw("GET", "/socket", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).Name("socket").WebSocket(),
	)
	if err != nil {
		t.Fatal(err)
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*projectManifest)
		valid  bool
	}{
		{name: "matching app and websocket", change: func(*projectManifest) {}, valid: true},
		{name: "missing manifest route", change: func(m *projectManifest) { m.Routes = m.Routes[:1] }},
		{name: "different route name", change: func(m *projectManifest) { m.Routes[0].Name = "beta" }},
		{name: "different policy", change: func(m *projectManifest) { m.Routes[0].Authenticated = true }},
		{name: "different status", change: func(m *projectManifest) { m.Routes[0].Status = 201 }},
		{name: "duplicate route", change: func(m *projectManifest) { m.Routes = append(m.Routes, m.Routes[0]) }},
		{name: "invalid method", change: func(m *projectManifest) { m.Routes[0].Method = "BOGUS" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := projectManifest{SchemaVersion: "trest.app.v1", Routes: app.Routes()}
			tc.change(&manifest)
			diagnostics := compareProjectManifest(manifest, document)
			if (len(diagnostics) == 0) != tc.valid {
				t.Fatalf("diagnostics: %+v", diagnostics)
			}
		})
	}
}

func TestProjectManifestRequiresVersionAndRoutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{name: "empty application", data: `{"schema_version":"trest.app.v1","routes":[]}`, valid: true},
		{name: "nil application routes", data: `{"schema_version":"trest.app.v1","routes":null}`, valid: true},
		{name: "missing routes", data: `{"schema_version":"trest.app.v1"}`},
		{name: "wrong version", data: `{"schema_version":"trest.inspect.v1","routes":[]}`},
		{name: "invalid routes", data: `{"schema_version":"trest.app.v1","routes":42}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeProjectManifest([]byte(tc.data))
			if (err == nil) != tc.valid {
				t.Fatalf("manifest error: %v", err)
			}
		})
	}
}

func TestProjectCheckRejectsConfigurationBeforeBuilding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "nonlocal package", args: []string{"--dir", "/missing", "--package", "example.com/remote"}},
		{name: "package without directory", args: []string{"--package", "./cmd/api"}},
		{name: "negative timeout", args: []string{"--dir", "/missing", "--timeout", "-1s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			if code := runCheck(context.Background(), tc.args, &output, &diagnostics); code != 2 {
				t.Fatalf("exit=%d output=%s diagnostics=%s", code, output.String(), diagnostics.String())
			}
		})
	}
	baseline := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(baseline, []byte(`{"openapi":"3.0.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	code := runCheck(context.Background(), []string{"--dir", "/missing", "--baseline", baseline, "--json"}, &output, &diagnostics)
	var report projectCheckReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if code != 1 || report.OK || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "invalid_baseline" || strings.Contains(diagnostics.String(), "go.mod") {
		t.Fatalf("invalid baseline reached project execution: %d %s %s", code, output.String(), diagnostics.String())
	}
}

func TestLegacyCheckPreservesFixtureBaseline(t *testing.T) {
	t.Parallel()
	var output, diagnostics bytes.Buffer
	if code := run(context.Background(), []string{"contract", "export", "--baseline"}, &output, &diagnostics); code != 0 {
		t.Fatalf("fixture export: %d %s", code, diagnostics.String())
	}
	baseline := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(baseline, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	code := run(context.Background(), []string{"check", "--baseline", baseline, "--json"}, &output, &diagnostics)
	var report projectCheckReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if code != 0 || !report.OK || report.Application != nil {
		t.Fatalf("fixture check changed: %d %s %s", code, output.String(), diagnostics.String())
	}
}
