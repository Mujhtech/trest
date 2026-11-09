// Package scenario runs explicitly registered application-authored local cases.
// It does not generate assertions, choose production endpoints, or infer consent
// from operation metadata. A case's code is trusted application code.
package scenario

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

const SchemaVersion = "trest.scenario.v1"

// Assertion is an application-authored, public-safe observation. Keep secrets,
// payloads, raw error messages, and database records out of Name.
type Assertion struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
}

type Case struct {
	Name    string
	Summary string
	Run     func(context.Context) ([]Assertion, error)
}

type Description struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

type Config struct {
	Enabled     bool
	Environment string
	Timeout     time.Duration
}

type Runner struct {
	config Config
	cases  map[string]Case
}

type Report struct {
	SchemaVersion string      `json:"schema_version"`
	Name          string      `json:"name"`
	Status        string      `json:"status"`
	Code          string      `json:"code,omitempty"`
	Message       string      `json:"message,omitempty"`
	Assertions    []Assertion `json:"assertions"`
	DurationNS    int64       `json:"duration_ns"`
}

var validName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,79}$`)

// New validates case registration. Listing cases is safe while execution is
// disabled. Only an explicit Enabled setting and local environment allow Run.
func New(config Config, cases ...Case) (*Runner, error) {
	if config.Timeout < 0 {
		return nil, errors.New("scenario: timeout must be positive")
	}
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}
	r := &Runner{config: config, cases: make(map[string]Case)}
	for _, c := range cases {
		if !validName.MatchString(c.Name) || c.Run == nil || c.Summary == "" || len(c.Summary) > 512 {
			return nil, errors.New("scenario: cases require a valid short name, summary, and function")
		}
		if _, exists := r.cases[c.Name]; exists {
			return nil, fmt.Errorf("scenario: duplicate case %q", c.Name)
		}
		r.cases[c.Name] = c
	}
	return r, nil
}

func (r *Runner) List() []Description {
	result := make([]Description, 0, len(r.cases))
	for _, c := range r.cases {
		result = append(result, Description{Name: c.Name, Summary: c.Summary})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Run returns bounded public observations. Cancellation is cooperative: case
// code must honor ctx; no goroutine is abandoned or side effect rolled back.
// A failed/panicking case never exposes the underlying error or panic value.
func (r *Runner) Run(ctx context.Context, name string) (report Report) {
	if !validName.MatchString(name) {
		return Report{SchemaVersion: SchemaVersion, Status: "failed", Code: "invalid_name", Message: "use a registered scenario name", Assertions: []Assertion{}}
	}
	report = Report{SchemaVersion: SchemaVersion, Name: name, Status: "failed", Assertions: []Assertion{}}
	fail := func(code, message string) Report { report.Code, report.Message = code, message; return report }
	if r == nil || !r.config.Enabled || r.config.Environment != "local" {
		return fail("execution_disabled", "local scenario execution must be explicitly enabled")
	}
	c, ok := r.cases[name]
	if !ok {
		return fail("unknown_scenario", "choose a registered scenario from the scenario list")
	}
	if ctx == nil {
		return fail("invalid_context", "provide an execution context")
	}
	ctx, cancel := context.WithTimeout(ctx, r.config.Timeout)
	defer cancel()
	if ctx.Err() != nil {
		return fail("canceled", "scenario context was canceled")
	}
	started := time.Now()
	defer func() {
		report.DurationNS = time.Since(started).Nanoseconds()
		if recover() != nil {
			report.Status, report.Code, report.Message = "failed", "scenario_panicked", "scenario failed with an internal panic"
			report.Assertions = []Assertion{}
		}
	}()
	assertions, err := c.Run(ctx)
	if ctx.Err() != nil {
		return fail("canceled", "scenario context was canceled or timed out")
	}
	if err != nil {
		return fail("scenario_failed", "scenario returned an error; inspect the application-owned local test")
	}
	if len(assertions) == 0 || len(assertions) > 128 {
		return fail("invalid_assertions", "scenario must return between 1 and 128 authored assertions")
	}
	passed := true
	for _, assertion := range assertions {
		if assertion.Name == "" || len(assertion.Name) > 256 {
			return fail("invalid_assertions", "assertion names must contain 1 through 256 public-safe bytes")
		}
		passed = passed && assertion.Passed
	}
	report.Assertions = append([]Assertion(nil), assertions...)
	if !passed {
		return fail("assertion_failed", "one or more application assertions failed")
	}
	report.Status = "passed"
	return report
}
