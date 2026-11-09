package check_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"trest.local/trest/check"
	"trest.local/trest/contract"
	"trest.local/trest/internal/example/invoiceapi"
)

type runner struct {
	calls [][]string
	exit  int
	err   error
}

func (r *runner) Run(_ context.Context, _ string, args []string) (int, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.exit, r.err
}

func TestRunExplicitChecks(t *testing.T) {
	t.Parallel()
	app, err := invoiceapi.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := &runner{}
	report := check.Run(context.Background(), app.Registry(), app.Bindings(), check.Config{Runner: executor})
	if !report.OK || len(executor.calls) != 0 {
		t.Fatalf("default check executed code: %+v", report)
	}
	report = check.Run(context.Background(), app.Registry(), app.Bindings(), check.Config{Runner: executor, Commands: []check.Command{{Kind: "test", Race: true}, {Kind: "vet"}}})
	if !report.OK || len(executor.calls) != 2 || strings.Join(executor.calls[0], " ") != "test -race ./..." {
		t.Fatalf("unexpected configured checks: %+v %+v", report, executor.calls)
	}
}

func TestRunRejectsCommandInjectionBeforeExecution(t *testing.T) {
	t.Parallel()
	app, err := invoiceapi.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []check.Command{{Kind: "run"}, {Kind: "test", Packages: []string{"-exec=sh"}}, {Kind: "test", Packages: []string{"./...; touch /tmp/unsafe"}}, {Kind: "build", Race: true}} {
		executor := &runner{}
		report := check.Run(context.Background(), app.Registry(), app.Bindings(), check.Config{Runner: executor, Commands: []check.Command{{Kind: "test"}, command}})
		if report.OK || len(executor.calls) != 0 {
			t.Fatalf("unsafe configuration executed: %+v", report)
		}
	}
}

func TestRunRedactsFailuresAndDetectsContractDrift(t *testing.T) {
	t.Parallel()
	app, err := invoiceapi.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := &runner{exit: 1, err: errors.New("password=secret")}
	report := check.Run(context.Background(), app.Registry(), app.Bindings(), check.Config{Runner: executor, Commands: []check.Command{{Kind: "test"}}})
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || strings.Contains(string(encoded), "secret") || report.Diagnostics[0].Code != "go_check_failed" {
		t.Fatalf("failure not safely reported: %s", encoded)
	}
	baseline, err := contract.Export(app.Registry(), app.Bindings(), contract.Info{Title: "Trest contract baseline", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	passing := check.Run(context.Background(), app.Registry(), app.Bindings(), check.Config{ContractBaseline: baseline})
	if !passing.OK {
		t.Fatalf("identical baseline failed: %+v", passing)
	}
	changed := check.Run(context.Background(), app.Registry(), app.Bindings(), check.Config{ContractBaseline: []byte(`{}`)})
	if changed.OK || changed.Diagnostics[0].Code != "contract_drift" {
		t.Fatalf("drift not detected: %+v", changed)
	}
}
