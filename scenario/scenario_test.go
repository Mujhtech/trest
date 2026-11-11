package scenario_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mujhtech/trest/scenario"
)

func TestExecutionRequiresLocalOptIn(t *testing.T) {
	t.Parallel()
	for _, config := range []scenario.Config{{}, {Enabled: true, Environment: "production"}, {Environment: "local"}} {
		called := false
		runner, err := scenario.New(config, scenario.Case{Name: "write", Summary: "local fixture", Run: func(context.Context) ([]scenario.Assertion, error) {
			called = true
			return []scenario.Assertion{{Name: "written", Passed: true}}, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(runner.List()) != 1 {
			t.Fatal("disabled cases should remain inspectable")
		}
		result := runner.Run(context.Background(), "write")
		if called || result.Code != "execution_disabled" {
			t.Fatalf("execution was not gated: %+v", result)
		}
	}
}

func TestRunAuthoredAssertionsAndRedactsFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fn   func(context.Context) ([]scenario.Assertion, error)
		want string
	}{
		{"passes", func(context.Context) ([]scenario.Assertion, error) {
			return []scenario.Assertion{{Name: "application invariant", Passed: true}}, nil
		}, ""},
		{"assertion-fails", func(context.Context) ([]scenario.Assertion, error) {
			return []scenario.Assertion{{Name: "application invariant", Passed: false}}, nil
		}, "assertion_failed"},
		{"no-assertions", func(context.Context) ([]scenario.Assertion, error) { return nil, nil }, "invalid_assertions"},
		{"error", func(context.Context) ([]scenario.Assertion, error) { return nil, errors.New("secret") }, "scenario_failed"},
		{"panic", func(context.Context) ([]scenario.Assertion, error) { panic("secret") }, "scenario_panicked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, err := scenario.New(scenario.Config{Enabled: true, Environment: "local"}, scenario.Case{Name: tc.name, Summary: "local fixture", Run: tc.fn})
			if err != nil {
				t.Fatal(err)
			}
			report := runner.Run(context.Background(), tc.name)
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if report.Code != tc.want || strings.Contains(string(data), "secret") {
				t.Fatalf("unexpected public report: %s", data)
			}
		})
	}
}

func TestCanceledCaseDoesNotRun(t *testing.T) {
	t.Parallel()
	called := false
	runner, err := scenario.New(scenario.Config{Enabled: true, Environment: "local"}, scenario.Case{Name: "cancel", Summary: "local fixture", Run: func(context.Context) ([]scenario.Assertion, error) { called = true; return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if report := runner.Run(ctx, "cancel"); report.Code != "canceled" || called {
		t.Fatalf("canceled case executed: %+v", report)
	}
}
