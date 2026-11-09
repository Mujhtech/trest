// Package check validates application metadata and runs explicitly configured Go
// checks. It never interprets a shell command or discovers executable checks from
// operation metadata. Running Go checks executes application-authored local code.
package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"trest.local/trest/contract"
	"trest.local/trest/httpadapter"
	"trest.local/trest/inspect"
	"trest.local/trest/operation"
)

const SchemaVersion = "trest.check.v1"

// Diagnostic is safe for machine-readable reports. Raw subprocess output and
// unclassified error messages are deliberately excluded.
type Diagnostic struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	OperationID string `json:"operation_id,omitempty"`
	Path        string `json:"path,omitempty"`
}

// Command is an allowlisted normal Go check, not shell text. Packages defaults
// to ./.... Only test accepts Race; arguments cannot inject Go flags.
type Command struct {
	Kind     string   `json:"kind"`
	Packages []string `json:"packages,omitempty"`
	Race     bool     `json:"race,omitempty"`
}

// Runner executes a Go argument vector; applications can inject their execution
// environment explicitly. A nil Runner uses the go executable on PATH.
type Runner interface {
	Run(context.Context, string, []string) (int, error)
}

type Config struct {
	Directory        string
	Commands         []Command
	Timeout          time.Duration
	ContractBaseline []byte
	Runner           Runner
}

type Result struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
}

type Report struct {
	SchemaVersion string       `json:"schema_version"`
	OK            bool         `json:"ok"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
	Checks        []Result     `json:"checks"`
}

var packagePattern = regexp.MustCompile(`^[A-Za-z0-9_./~-]+$`)

func arguments(c Command) ([]string, error) {
	switch c.Kind {
	case "test", "vet", "build":
	default:
		return nil, errors.New("only go test, vet, and build are supported")
	}
	args := []string{c.Kind}
	if c.Race {
		if c.Kind != "test" {
			return nil, errors.New("race is only supported for go test")
		}
		args = append(args, "-race")
	}
	packages := c.Packages
	if len(packages) == 0 {
		packages = []string{"./..."}
	}
	for _, pattern := range packages {
		if strings.HasPrefix(pattern, "-") || !packagePattern.MatchString(pattern) {
			return nil, errors.New("package patterns cannot contain flags, spaces, or shell syntax")
		}
		args = append(args, pattern)
	}
	return args, nil
}

// Run performs metadata and contract checks, then configured Go checks in order.
// Without Commands, no application code or subprocess runs. A baseline mismatch
// is a review signal rather than an automatic claim of a breaking API change.
func Run(ctx context.Context, registry *operation.Registry, bindings []httpadapter.Binding, config Config) Report {
	report := Report{SchemaVersion: SchemaVersion, OK: true, Diagnostics: []Diagnostic{}, Checks: []Result{}}
	add := func(d Diagnostic) { report.OK = false; report.Diagnostics = append(report.Diagnostics, d) }
	if ctx == nil {
		add(Diagnostic{Code: "invalid_config", Message: "provide a context"})
		return report
	}
	if config.Timeout < 0 {
		add(Diagnostic{Code: "invalid_config", Message: "check timeout must be positive"})
		return report
	}
	commands := make([][]string, len(config.Commands))
	for i, command := range config.Commands {
		args, err := arguments(command)
		if err != nil {
			add(Diagnostic{Code: "invalid_check", Message: err.Error(), Path: fmt.Sprintf("commands/%d", i)})
		} else {
			commands[i] = args
		}
	}
	if !report.OK {
		return report // Configuration rejection must occur before any execution.
	}
	if _, err := inspect.Build(registry, bindings); err != nil {
		add(FromError(err))
		report.Checks = append(report.Checks, Result{Name: "registry_and_bindings", Status: "failed", ExitCode: 1})
		return report
	}
	report.Checks = append(report.Checks, Result{Name: "registry_and_bindings", Status: "passed"})
	if config.ContractBaseline != nil {
		// Baselines are exported with a fixed metadata header so comparison does
		// not depend on the application's display title or release version.
		current, err := contract.Export(registry, bindings, contract.Info{Title: "Trest contract baseline", Version: "1"})
		if err != nil {
			add(FromError(err))
		} else if strings.TrimSpace(string(current)) != strings.TrimSpace(string(config.ContractBaseline)) {
			add(Diagnostic{Code: "contract_drift", Message: "generated contract differs from the baseline; review the change and deliberately update it"})
		}
		status := "passed"
		if !report.OK {
			status = "failed"
		}
		report.Checks = append(report.Checks, Result{Name: "contract_baseline", Status: status})
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runner := config.Runner
	if runner == nil {
		runner = goRunner{}
	}
	for _, args := range commands {
		if ctx.Err() != nil {
			add(Diagnostic{Code: "check_canceled", Message: "checks stopped because their context was canceled or timed out"})
			break
		}
		exitCode, err := runner.Run(ctx, config.Directory, args)
		result := Result{Name: "go " + strings.Join(args, " "), Status: "passed", ExitCode: exitCode}
		if err != nil || exitCode != 0 {
			result.Status = "failed"
			if exitCode == 0 {
				result.ExitCode = -1
			}
			code := "go_check_failed"
			if ctx.Err() != nil {
				code = "check_canceled"
			}
			add(Diagnostic{Code: code, Message: "local Go check failed; run the named check directly for local compiler/test output", Path: result.Name})
		}
		report.Checks = append(report.Checks, result)
	}
	return report
}

// FromError preserves known startup diagnostics and redacts unknown errors.
func FromError(err error) Diagnostic {
	var d *operation.Diagnostic
	if errors.As(err, &d) && d != nil {
		return Diagnostic{Code: d.Code, Message: d.Message, OperationID: d.OperationID, Path: d.Path}
	}
	return Diagnostic{Code: "invalid_application", Message: "application construction or binding validation failed; inspect the local startup error"}
}

type goRunner struct{}

func (goRunner) Run(ctx context.Context, directory string, args []string) (int, error) {
	command := exec.CommandContext(ctx, "go", args...)
	command.Dir = directory
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.WaitDelay = time.Second
	err := command.Run()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), err
	}
	return -1, err
}
