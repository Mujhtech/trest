package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDeveloperLoop(t *testing.T) {
	var output, diagnostics bytes.Buffer
	invoke := func(args ...string) int {
		output.Reset()
		diagnostics.Reset()
		return run(context.Background(), args, &output, &diagnostics)
	}
	if code := invoke("inspect", "--json"); code != 0 || !bytes.Contains(output.Bytes(), []byte(`"source"`)) {
		t.Fatalf("inspect failed: %d %s %s", code, output.String(), diagnostics.String())
	}
	if code := invoke("check", "--json"); code != 0 || !bytes.Contains(output.Bytes(), []byte(`"ok": true`)) {
		t.Fatalf("check failed: %d %s", code, output.String())
	}
	if code := invoke("scenario", "run", "invoice-create", "--json"); code != 1 || !bytes.Contains(output.Bytes(), []byte("execution_disabled")) {
		t.Fatalf("ungated scenario: %d %s", code, output.String())
	}
	tracePath := filepath.Join(t.TempDir(), "traces.json")
	if code := invoke("scenario", "run", "invoice-create", "--local", "--json", "--trace-file", tracePath); code != 0 {
		t.Fatalf("scenario failed: %d %s %s", code, output.String(), diagnostics.String())
	}
	var result struct {
		TraceIDs []string `json:"trace_ids"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.TraceIDs) != 1 {
		t.Fatalf("missing invocation trace: %s", output.String())
	}
	info, err := os.Stat(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("trace file permissions: %v", info.Mode())
	}
	if code := invoke("trace", "show", tracePath, result.TraceIDs[0], "--json"); code != 0 || !bytes.Contains(output.Bytes(), []byte(`"phase": "handler"`)) {
		t.Fatalf("trace read failed: %d %s", code, output.String())
	}
	for _, name := range []string{"invoice-denied", "invoice-validation"} {
		if code := invoke("scenario", "run", name, "--local", "--json"); code != 0 {
			t.Fatalf("%s failed: %d %s", name, code, output.String())
		}
	}
}

func TestTraceFilesAreBoundedAndNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.json")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveTraces(path, nil); err == nil {
		t.Fatal("overwrote existing file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing" {
		t.Fatal("existing data changed")
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), maxLocalFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBounded(path); err == nil {
		t.Fatal("oversize trace accepted")
	}
}
