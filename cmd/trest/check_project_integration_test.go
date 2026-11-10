//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectChecksSelectApplicationAndDetectContractDrift(t *testing.T) {
	directories := map[string]string{}
	for _, name := range []string{"alpha", "beta"} {
		directory := newProject(t)
		for _, file := range []string{"main.go", "main_test.go"} {
			path := filepath.Join(directory, "cmd", "server", file)
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			source = bytes.ReplaceAll(source, []byte("/greet/"), []byte("/"+name+"/"))
			source = bytes.ReplaceAll(source, []byte(`Name("greet")`), []byte(`Name("`+name+`")`))
			if err := os.WriteFile(path, source, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Rename(filepath.Join(directory, "cmd", "server"), filepath.Join(directory, "cmd", "api")); err != nil {
			t.Fatal(err)
		}
		directories[name] = directory
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var output, diagnostics bytes.Buffer
	invoke := func(args ...string) int {
		t.Helper()
		output.Reset()
		diagnostics.Reset()
		return run(ctx, args, &output, &diagnostics)
	}
	baseline := filepath.Join(t.TempDir(), "alpha.openapi.json")
	if code := invoke("contract", "export", "--dir", directories["alpha"], "--package", "./cmd/api", "--baseline", "--out", baseline); code != 0 {
		t.Fatalf("baseline export: %d %s %s", code, output.String(), diagnostics.String())
	}
	checkApp := func(name string, extra ...string) (int, projectCheckReport) {
		t.Helper()
		args := []string{"check", "--dir", directories[name], "--package", "./cmd/api", "--json"}
		code := invoke(append(args, extra...)...)
		var report projectCheckReport
		if err := json.Unmarshal(output.Bytes(), &report); err != nil {
			t.Fatalf("check JSON: %v %s %s", err, output.String(), diagnostics.String())
		}
		if report.Application == nil {
			t.Fatalf("check omitted selected app: %s %s", output.String(), diagnostics.String())
		}
		found := false
		for _, route := range report.Application.Routes {
			if route.Path == "/"+name+"/{name}" && route.Name == name {
				found = true
			}
			if strings.Contains(route.Path, "invoice") || name == "alpha" && strings.Contains(route.Path, "/beta/") || name == "beta" && strings.Contains(route.Path, "/alpha/") {
				t.Fatalf("check used another application's metadata: %+v", report.Application.Routes)
			}
		}
		if !found {
			t.Fatalf("selected route missing: %+v", report.Application.Routes)
		}
		return code, report
	}
	if code, report := checkApp("alpha", "--baseline", baseline, "--run-go-checks"); code != 0 || !report.OK {
		t.Fatalf("alpha check: %d %s %s", code, output.String(), diagnostics.String())
	}
	if code, report := checkApp("beta"); code != 0 || !report.OK {
		t.Fatalf("beta check: %d %s %s", code, output.String(), diagnostics.String())
	}
	assertDrift := func(name string) {
		t.Helper()
		code, report := checkApp(name, "--baseline", baseline)
		if code != 1 || report.OK || len(report.Diagnostics) != 1 || report.Diagnostics[0].Code != "contract_drift" {
			t.Fatalf("missing contract drift for %s: %d %s %s", name, code, output.String(), diagnostics.String())
		}
	}
	assertDrift("beta")
	path := filepath.Join(directories["alpha"], "cmd", "api", "main.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	displayChange := bytes.Replace(source, []byte(`trest.Info("My Trest app", "0.1.0")`), []byte(`trest.Info("Renamed application", "2.0.0")`), 1)
	if bytes.Equal(source, displayChange) {
		t.Fatal("display metadata fixture did not change")
	}
	if err := os.WriteFile(path, displayChange, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, report := checkApp("alpha", "--baseline", baseline); code != 0 || !report.OK {
		t.Fatalf("display metadata caused drift: %d %s %s", code, output.String(), diagnostics.String())
	}
	schemaChange := bytes.Replace(displayChange, []byte(`json:"message"`), []byte(`json:"greeting"`), 1)
	if bytes.Equal(displayChange, schemaChange) {
		t.Fatal("response schema fixture did not change")
	}
	if err := os.WriteFile(path, schemaChange, 0o644); err != nil {
		t.Fatal(err)
	}
	assertDrift("alpha")
	invalid := bytes.Replace(schemaChange, []byte(`minLength:"2"`), []byte(`minLength:"broken"`), 1)
	if err := os.WriteFile(path, invalid, 0o644); err != nil {
		t.Fatal(err)
	}
	code := invoke("check", "--dir", directories["alpha"], "--package", "./cmd/api", "--json")
	var failed projectCheckReport
	if err := json.Unmarshal(output.Bytes(), &failed); err != nil {
		t.Fatal(err)
	}
	if code != 1 || failed.OK || len(failed.Diagnostics) != 1 || failed.Diagnostics[0].Code != "application_export_failed" || !strings.Contains(diagnostics.String(), "minLength") {
		t.Fatalf("construction failure hidden: %d %s %s", code, output.String(), diagnostics.String())
	}
}
