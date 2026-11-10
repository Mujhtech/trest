package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"trest.local/trest"
	"trest.local/trest/check"
	"trest.local/trest/internal/example/invoiceapi"
)

type projectManifest struct {
	SchemaVersion string            `json:"schema_version"`
	Routes        []trest.RouteInfo `json:"routes"`
}

type projectCheckReport struct {
	check.Report
	Application *projectManifest `json:"application,omitempty"`
}

func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	set := flags("check", stderr)
	jsonOutput := set.Bool("json", false, "print versioned JSON")
	runGo := set.Bool("run-go-checks", false, "explicitly run local go test -race, vet, and build")
	directory := set.String("dir", "", "check a local application's exports (omitted: invoice fixture)")
	packagePath := set.String("package", "", "local app main package (with --dir: ./cmd/server)")
	timeout := set.Duration("timeout", 2*time.Minute, "total export and Go check timeout")
	baseline := set.String("baseline", "", "compare a previously exported contract baseline")
	if set.Parse(args) != nil || set.NArg() != 0 || *timeout < 0 {
		return 2
	}
	if *timeout == 0 {
		*timeout = 2 * time.Minute
	}
	if *directory == "" && *packagePath != "" {
		_, _ = fmt.Fprintln(stderr, "--package requires --dir")
		return 2
	}
	if *packagePath == "" {
		*packagePath = "./cmd/server"
	}
	if *directory != "" && !validPackageTarget(*packagePath) {
		_, _ = fmt.Fprintln(stderr, "--package must name one local main package")
		return 2
	}
	var baselineData []byte
	if *baseline != "" {
		var err error
		baselineData, err = readBounded(*baseline)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "cannot read bounded local baseline:", err)
			return 1
		}
	}
	var report projectCheckReport
	if *directory != "" {
		bounded, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		report = checkProject(bounded, *directory, *packagePath, baselineData, *runGo, stderr)
	} else {
		app, err := invoiceapi.New(nil)
		if err != nil {
			report.Report = check.Report{SchemaVersion: check.SchemaVersion, OK: false, Diagnostics: []check.Diagnostic{check.FromError(err)}, Checks: []check.Result{}}
		} else {
			config := check.Config{Directory: ".", Timeout: *timeout, ContractBaseline: baselineData}
			if *runGo {
				config.Commands = []check.Command{{Kind: "test", Race: true}, {Kind: "vet"}, {Kind: "build"}}
			}
			report.Report = check.Run(ctx, app.Registry(), app.Bindings(), config)
		}
	}
	if *jsonOutput {
		if err := writeJSON(stdout, report); err != nil {
			return 1
		}
	} else {
		if report.Application != nil {
			_, _ = fmt.Fprintf(stdout, "application: %d routes from %s\n", len(report.Application.Routes), *packagePath)
		}
		for _, result := range report.Checks {
			_, _ = fmt.Fprintf(stdout, "%s: %s\n", result.Status, result.Name)
		}
		for _, diagnostic := range report.Diagnostics {
			_, _ = fmt.Fprintf(stdout, "%s: %s (%s)\n", diagnostic.Code, diagnostic.Message, diagnostic.Path)
		}
	}
	if !report.OK {
		return 1
	}
	return 0
}

func checkProject(ctx context.Context, directory, target string, baseline []byte, runGo bool, stderr io.Writer) projectCheckReport {
	report := projectCheckReport{Report: check.Report{SchemaVersion: check.SchemaVersion, OK: true, Diagnostics: []check.Diagnostic{}, Checks: []check.Result{}}}
	fail := func(name, code, message, path string) {
		report.OK = false
		report.Diagnostics = append(report.Diagnostics, check.Diagnostic{Code: code, Message: message, Path: path})
		report.Checks = append(report.Checks, check.Result{Name: name, Status: "failed", ExitCode: 1})
	}
	if baseline != nil {
		var err error
		baseline, err = normalizeProjectContract(baseline)
		if err != nil {
			fail("contract_baseline", "invalid_baseline", "baseline must be a valid OpenAPI 3.1 document", "")
			return report
		}
	}
	manifestData, contractData, err := exportProjectCheck(ctx, directory, target, stderr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot check application:", err)
		code := "application_export_failed"
		if ctx.Err() != nil {
			code = "check_canceled"
		}
		fail("application_exports", code, "selected application could not be built or exported; inspect the local diagnostics", target)
		return report
	}
	report.Checks = append(report.Checks, check.Result{Name: "application_exports", Status: "passed"})
	manifest, err := decodeProjectManifest(manifestData)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot check manifest:", err)
		fail("manifest_and_contract", "invalid_manifest", "application must export a trest.app.v1 manifest with routes", target)
		return report
	}
	report.Application = &manifest
	current, err := normalizeProjectContract(contractData)
	if err != nil {
		fail("manifest_and_contract", "invalid_contract", "application must export an OpenAPI 3.1 document with info and paths objects", target)
		return report
	}
	if diagnostics := compareProjectManifest(manifest, current); len(diagnostics) > 0 {
		report.OK = false
		report.Diagnostics = append(report.Diagnostics, diagnostics...)
		report.Checks = append(report.Checks, check.Result{Name: "manifest_and_contract", Status: "failed", ExitCode: 1})
		return report
	}
	report.Checks = append(report.Checks, check.Result{Name: "manifest_and_contract", Status: "passed"})
	if baseline != nil {
		if !bytes.Equal(current, baseline) {
			fail("contract_baseline", "contract_drift", "generated contract differs from the baseline; review the change and deliberately update it", "")
		} else {
			report.Checks = append(report.Checks, check.Result{Name: "contract_baseline", Status: "passed"})
		}
	}
	if runGo {
		runProjectGoChecks(ctx, directory, &report.Report, stderr)
	}
	return report
}

// Build once so both exports describe the same application binary.
func exportProjectCheck(ctx context.Context, directory, target string, stderr io.Writer) ([]byte, []byte, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Stat(filepath.Join(absolute, "go.mod")); err != nil {
		return nil, nil, errors.New("project directory must contain go.mod")
	}
	temporary, err := os.MkdirTemp("", "trest-check-")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	binary := filepath.Join(temporary, "app")
	if err := buildProject(ctx, absolute, binary, target, stderr); err != nil {
		return nil, nil, err
	}
	manifest, err := runExport(ctx, binary, absolute, "manifest", stderr)
	if err != nil {
		return nil, nil, err
	}
	document, err := runExport(ctx, binary, absolute, "openapi", stderr)
	return manifest, document, err
}

func decodeProjectManifest(data []byte) (projectManifest, error) {
	var manifest projectManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return projectManifest{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return projectManifest{}, err
	}
	if manifest.SchemaVersion != "trest.app.v1" || len(fields["routes"]) == 0 {
		return projectManifest{}, errors.New("expected trest.app.v1 and routes")
	}
	return manifest, nil
}

// Compare canonical JSON without losing large integer schema bounds. Display
// titles and release versions are excluded; all other contract fields remain.
func normalizeProjectContract(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("contract must contain one JSON document")
	}
	version, _ := document["openapi"].(string)
	info, infoOK := document["info"].(map[string]any)
	_, pathsOK := document["paths"].(map[string]any)
	if !strings.HasPrefix(version, "3.1.") || !infoOK || !pathsOK {
		return nil, errors.New("contract requires OpenAPI 3.1, info, and paths")
	}
	info["title"] = "Trest contract baseline"
	info["version"] = "1"
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

var projectCapture = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)(?:\.\.\.)?\}`)
var projectRouteName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

func compareProjectManifest(manifest projectManifest, data []byte) []check.Diagnostic {
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return []check.Diagnostic{{Code: "invalid_contract", Message: "contract paths must contain path item objects"}}
	}
	diagnostics := []check.Diagnostic{}
	add := func(code, message, path string) {
		diagnostics = append(diagnostics, check.Diagnostic{Code: code, Message: message, Path: path})
	}
	routes, names := map[string]bool{}, map[string]bool{}
	for _, route := range manifest.Routes {
		path := projectCapture.ReplaceAllString(route.Path, "{$1}")
		key := route.Method + " " + path
		if !projectMethod(route.Method) || !strings.HasPrefix(path, "/") || !projectRouteName.MatchString(route.Name) || routes[key] || names[route.Name] {
			add("invalid_manifest", "route method, path, and unique identifier name are required", key)
			continue
		}
		routes[key], names[route.Name] = true, true
		var operation struct {
			Name          string                     `json:"operationId"`
			Responses     map[string]json.RawMessage `json:"responses"`
			OperationID   string                     `json:"x-trest-operation"`
			Authenticated bool                       `json:"x-trest-authenticated"`
			Permission    string                     `json:"x-trest-permission"`
			NativeOnly    bool                       `json:"x-trest-native"`
			Protocol      string                     `json:"x-trest-protocol"`
		}
		encoded := document.Paths[path][strings.ToLower(route.Method)]
		if len(encoded) == 0 || json.Unmarshal(encoded, &operation) != nil {
			add("manifest_contract_mismatch", "manifest route is missing or invalid in the contract", key)
			continue
		}
		validStatus := route.Status >= 200 && route.Status < 400 || route.Protocol == "websocket" && route.Status == http.StatusSwitchingProtocols
		if !validStatus || len(operation.Responses[strconv.Itoa(route.Status)]) == 0 || operation.Name != route.Name || operation.OperationID != route.OperationID || operation.Authenticated != route.Authenticated || operation.Permission != route.Permission || operation.NativeOnly != route.NativeOnly || operation.Protocol != route.Protocol {
			add("manifest_contract_mismatch", "route identity, policy, protocol, or success status differs between manifest and contract", key)
		}
	}
	for path, item := range document.Paths {
		for method := range item {
			method = strings.ToUpper(method)
			key := method + " " + path
			if projectMethod(method) && !routes[key] {
				add("manifest_contract_mismatch", "contract route is missing from the manifest", key)
			}
		}
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].Path != diagnostics[j].Path {
			return diagnostics[i].Path < diagnostics[j].Path
		}
		if diagnostics[i].Code != diagnostics[j].Code {
			return diagnostics[i].Code < diagnostics[j].Code
		}
		return diagnostics[i].Message < diagnostics[j].Message
	})
	return diagnostics
}

func projectMethod(method string) bool {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return true
	default:
		return false
	}
}

func runProjectGoChecks(ctx context.Context, directory string, report *check.Report, stderr io.Writer) {
	for _, args := range [][]string{{"test", "-race", "./..."}, {"vet", "./..."}, {"build", "./..."}} {
		name := "go " + strings.Join(args, " ")
		if ctx.Err() != nil {
			report.OK = false
			report.Diagnostics = append(report.Diagnostics, check.Diagnostic{Code: "check_canceled", Message: "checks stopped because their context was canceled or timed out", Path: name})
			return
		}
		command := exec.CommandContext(ctx, "go", args...)
		command.Dir = directory
		command.Env = projectEnvironment("")
		command.Stdout, command.Stderr = stderr, stderr
		command.WaitDelay = time.Second
		configureBuildCancellation(command)
		result := check.Result{Name: name, Status: "passed"}
		if err := command.Run(); err != nil {
			result.Status, result.ExitCode = "failed", -1
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				result.ExitCode = exit.ExitCode()
			}
			report.OK = false
			code := "go_check_failed"
			if ctx.Err() != nil {
				code = "check_canceled"
			}
			report.Diagnostics = append(report.Diagnostics, check.Diagnostic{Code: code, Message: "local Go check failed; inspect the local diagnostics", Path: name})
		}
		report.Checks = append(report.Checks, result)
	}
}
