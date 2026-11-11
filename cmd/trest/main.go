// Command trest scaffolds, runs, inspects, and generates clients for local apps.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mujhtech/trest/check"
	"github.com/mujhtech/trest/contract"
	"github.com/mujhtech/trest/inspect"
	"github.com/mujhtech/trest/internal/example/invoiceapi"
	"github.com/mujhtech/trest/scenario"
	"github.com/mujhtech/trest/telemetry"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func flags(name string, stderr io.Writer) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(stderr)
	return set
}

func writeJSON(w io.Writer, v any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: trest new | dev | inspect | contract export | client | check | scenario list/run | trace show/list")
		return 2
	}
	if args[0] == "new" {
		return runNew(args[1:], stdout, stderr)
	}
	if args[0] == "client" {
		return runClient(args[1:], stdout, stderr)
	}
	if args[0] == "dev" {
		return runDev(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "check" {
		return runCheck(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "scenario" {
		return runScenario(ctx, args[1:], stdout, stderr)
	}
	if args[0] == "trace" {
		return runTrace(args[1:], stdout, stderr)
	}
	app, err := invoiceapi.New(nil)
	if err != nil {
		_ = writeJSON(stdout, check.Report{SchemaVersion: check.SchemaVersion, OK: false, Diagnostics: []check.Diagnostic{check.FromError(err)}, Checks: []check.Result{}})
		return 1
	}
	switch args[0] {
	case "inspect":
		set := flags("inspect", stderr)
		jsonOutput := set.Bool("json", false, "print versioned JSON")
		directory := set.String("dir", "", "inspect a local application's -manifest export")
		packagePath := set.String("package", "./cmd/server", "local app main package")
		if set.Parse(args[1:]) != nil || set.NArg() != 0 {
			return 2
		}
		if *directory != "" {
			data, err := exportProject(ctx, *directory, *packagePath, "manifest", stderr)
			if err != nil {
				_, _ = fmt.Fprintln(stderr, err)
				return 1
			}
			var manifest struct {
				SchemaVersion string                                         `json:"schema_version"`
				Routes        []struct{ Name, Method, Path, Summary string } `json:"routes"`
			}
			if err := json.Unmarshal(data, &manifest); err != nil || manifest.SchemaVersion != "trest.app.v1" {
				_, _ = fmt.Fprintln(stderr, "application must export a trest.app.v1 manifest")
				return 1
			}
			if *jsonOutput {
				if _, err := stdout.Write(data); err != nil {
					return 1
				}
			} else {
				for _, route := range manifest.Routes {
					_, _ = fmt.Fprintf(stdout, "%s %s  %s  %s\n", route.Method, route.Path, route.Name, route.Summary)
				}
			}
			return 0
		}
		model, err := inspect.Build(app.Registry(), app.Bindings())
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "cannot inspect the application")
			return 1
		}
		if *jsonOutput {
			if err := writeJSON(stdout, model); err != nil {
				return 1
			}
		} else {
			_, _ = fmt.Fprintf(stdout, "%s: %d operations, %d HTTP bindings\n", model.SchemaVersion, len(model.Operations), len(model.HTTP))
			for _, op := range model.Operations {
				_, _ = fmt.Fprintf(stdout, "%s  %s  %s:%d  %s\n", op.ID, op.Access.Mode, op.Source.File, op.Source.Line, op.Summary)
			}
			for _, binding := range model.HTTP {
				_, _ = fmt.Fprintf(stdout, "%s %s -> %s\n", binding.Method, binding.Path, binding.OperationID)
			}
		}
	case "contract":
		if len(args) < 2 || args[1] != "export" {
			return 2
		}
		set := flags("contract export", stderr)
		format := set.String("format", "openapi", "contract format (openapi)")
		baseline := set.Bool("baseline", false, "use stable check-baseline metadata")
		directory := set.String("dir", "", "export a local application's -openapi output")
		packagePath := set.String("package", "./cmd/server", "local app main package")
		output := set.String("out", "-", "output file, or - for stdout")
		overwrite := set.Bool("overwrite", false, "explicitly replace an existing output file")
		if set.Parse(args[2:]) != nil || set.NArg() != 0 || *format != "openapi" {
			return 2
		}
		var data []byte
		if *directory != "" {
			data, err = exportProject(ctx, *directory, *packagePath, "openapi", stderr)
			if err == nil && *baseline {
				data, err = normalizeProjectContract(data)
			}
		} else if *baseline {
			data, err = contract.Export(app.Registry(), app.Bindings(), contract.Info{Title: "Trest contract baseline", Version: "1"})
		} else {
			data, err = app.OpenAPI()
		}
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "cannot export the contract:", err)
			return 1
		}
		if *output != "-" {
			if err := writeGenerated(*output, data, *overwrite, false); err != nil {
				_, _ = fmt.Fprintln(stderr, "cannot write contract:", err)
				return 1
			}
			_, _ = fmt.Fprintln(stdout, "Exported", *output)
			return 0
		}
		if _, err := stdout.Write(append(data, '\n')); err != nil {
			return 1
		}
	default:
		_, _ = fmt.Fprintln(stderr, "unknown command")
		return 2
	}
	return 0
}

func runScenario(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return 2
	}
	set := flags("scenario", stderr)
	jsonOutput := set.Bool("json", false, "print versioned JSON")
	local := set.Bool("local", false, "explicitly enable the application-authored in-memory fixture")
	traceFile := set.String("trace-file", "", "write metadata traces to a new private local file")
	name, remaining := "", args[1:]
	if args[0] == "run" {
		if len(args) < 2 {
			return 2
		}
		name, remaining = args[1], args[2:]
	} else if args[0] != "list" {
		return 2
	}
	if set.Parse(remaining) != nil || set.NArg() != 0 {
		return 2
	}
	recorder, err := telemetry.NewRecorder(512)
	if err != nil {
		return 1
	}
	runner, err := scenario.New(scenario.Config{Enabled: *local, Environment: "local", Timeout: 10 * time.Second}, invoiceCases(recorder)...)
	if err != nil {
		return 1
	}
	if args[0] == "list" {
		if *jsonOutput {
			err = writeJSON(stdout, struct {
				SchemaVersion string                 `json:"schema_version"`
				Cases         []scenario.Description `json:"cases"`
			}{scenario.SchemaVersion, runner.List()})
		} else {
			for _, description := range runner.List() {
				_, _ = fmt.Fprintf(stdout, "%s: %s\n", description.Name, description.Summary)
			}
		}
		if err != nil {
			return 1
		}
		return 0
	}
	report := runner.Run(ctx, name)
	traces := recorder.Traces()
	if *traceFile != "" && len(traces) > 0 {
		if err := saveTraces(*traceFile, traces); err != nil {
			_, _ = fmt.Fprintln(stderr, "cannot create private trace file; choose a new local path")
			return 1
		}
	}
	type result struct {
		scenario.Report
		TraceIDs []string `json:"trace_ids"`
	}
	output := result{Report: report, TraceIDs: []string{}}
	for _, trace := range traces {
		output.TraceIDs = append(output.TraceIDs, trace.ID)
	}
	if *jsonOutput {
		err = writeJSON(stdout, output)
	} else {
		_, _ = fmt.Fprintf(stdout, "%s: %s (%s)\n", report.Name, report.Status, report.Code)
		for _, assertion := range report.Assertions {
			_, _ = fmt.Fprintf(stdout, "  %t %s\n", assertion.Passed, assertion.Name)
		}
		for _, id := range output.TraceIDs {
			_, _ = fmt.Fprintf(stdout, "trace %s\n", id)
		}
	}
	if err != nil || report.Status != "passed" {
		return 1
	}
	return 0
}

const maxLocalFile = 4 << 20

type traceEnvelope struct {
	SchemaVersion string            `json:"schema_version"`
	Traces        []telemetry.Trace `json:"traces"`
}

func saveTraces(path string, traces []telemetry.Trace) error {
	data, err := json.MarshalIndent(traceEnvelope{SchemaVersion: "trest.traces.v1", Traces: traces}, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxLocalFile {
		return errors.New("trace file is too large")
	}
	// Exclusive creation avoids following an existing symlink or overwriting a
	// different file; mode 0600 keeps identity metadata private by default.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	return errors.Join(writeErr, file.Close())
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("local input must be a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLocalFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxLocalFile {
		return nil, errors.New("local file exceeds limit")
	}
	return data, nil
}

func runTrace(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		return 2
	}
	kind, path := args[0], args[1]
	id, remaining := "", args[2:]
	if kind == "show" {
		if len(args) < 3 {
			return 2
		}
		id, remaining = args[2], args[3:]
	} else if kind != "list" {
		return 2
	}
	set := flags("trace", stderr)
	jsonOutput := set.Bool("json", false, "print versioned JSON")
	if set.Parse(remaining) != nil || set.NArg() != 0 {
		return 2
	}
	data, err := readBounded(path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot read bounded local trace file")
		return 1
	}
	var envelope traceEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || envelope.SchemaVersion != "trest.traces.v1" {
		return 1
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return 1
	}
	if kind == "show" {
		found := false
		for _, trace := range envelope.Traces {
			if trace.ID == id {
				envelope.Traces = []telemetry.Trace{trace}
				found = true
				break
			}
		}
		if !found {
			_, _ = fmt.Fprintln(stderr, "trace not found in the local file")
			return 1
		}
	}
	if *jsonOutput {
		if writeJSON(stdout, envelope) != nil {
			return 1
		}
	} else {
		for _, trace := range envelope.Traces {
			_, _ = fmt.Fprintf(stdout, "trace %s (%d retained events)\n", trace.ID, len(trace.Events))
			if kind == "show" {
				for _, event := range trace.Events {
					_, _ = fmt.Fprintf(stdout, "  %s %s outcome=%s duration=%s request=%s\n", event.OperationID, event.Phase, event.Code, event.Duration, event.CorrelationID)
				}
			}
		}
	}
	return 0
}
