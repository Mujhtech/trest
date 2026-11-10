package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The dev process executes explicitly selected local Go applications. No command
// text comes from application metadata and no shell command is interpreted.
type devConfig struct {
	directory, address string
	packagePath        string
	watch              bool
	poll, buildTimeout time.Duration
	output             io.Writer
}
type devProcess interface {
	Done() <-chan struct{}
	Err() error
	Stop(context.Context) error
}
type devDependencies struct {
	build      func(context.Context, string, string) error
	start      func(string, string, io.Writer) (devProcess, error)
	ready      func(context.Context, string, string) error
	afterBuild func(context.Context, string, string) error
}

func runDev(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	set := flags("dev", stderr)
	address := set.String("addr", "127.0.0.1:8080", "loopback listen address")
	directory := set.String("dir", "", "local app directory (omitted: invoice fixture)")
	packagePath := set.String("package", "", "single local main package (with --dir: ./cmd/server)")
	readyPath := set.String("ready-path", "/readyz", "readiness route with X-Trest-Dev-Instance response marker")
	clientLanguage := set.String("client", "", "regenerate a ts or go client after each successful build")
	clientOutput := set.String("client-out", "", "generated client path, relative to the app directory")
	clientPackage := set.String("client-package", "client", "generated Go client package")
	watch := set.Bool("watch", false, "explicitly rebuild and restart after local Go source changes")
	poll := set.Duration("poll", 500*time.Millisecond, "source polling interval, at least 50ms")
	buildTimeout := set.Duration("build-timeout", time.Minute, "maximum duration for each build")
	if set.Parse(args) != nil || set.NArg() != 0 {
		return 2
	}
	project := *directory != "" || *packagePath != ""
	if *directory == "" {
		*directory = "."
	}
	if *packagePath == "" {
		if project {
			*packagePath = "./cmd/server"
		} else {
			*packagePath = "./cmd/invoices"
		}
	}
	if !validPackageTarget(*packagePath) || !validReadyPath(*readyPath) || (*clientLanguage != "" && *clientLanguage != "ts" && *clientLanguage != "go") || (*clientLanguage == "") != (*clientOutput == "") {
		return 2
	}
	diagnostics := &lockedWriter{writer: stderr}
	deps := devDependencies{
		build: func(ctx context.Context, directory, binary string) error {
			env := devEnvironment()
			if project {
				env = projectEnvironment("")
			}
			return buildTarget(ctx, directory, binary, *packagePath, env, diagnostics)
		},
		start: func(binary, address string, _ io.Writer) (devProcess, error) {
			env := devEnvironment()
			if project {
				env = projectEnvironment("")
			}
			return startTarget(binary, address, *directory, env, diagnostics)
		},
		ready: func(ctx context.Context, address, instance string) error {
			return waitReadyPath(ctx, address, instance, *readyPath, !project)
		},
	}
	if *clientLanguage != "" {
		deps.afterBuild = func(ctx context.Context, binary, directory string) (failure error) {
			defer func() {
				if failure != nil {
					_, _ = fmt.Fprintln(diagnostics, "client generation failed:", failure)
				}
			}()
			document, err := runExport(ctx, binary, directory, "openapi", diagnostics)
			if err != nil {
				return err
			}
			generated, err := generateClient(document, *clientLanguage, *clientPackage)
			if err != nil {
				return err
			}
			path := *clientOutput
			if !filepath.IsAbs(path) {
				path = filepath.Join(directory, path)
			}
			return writeGenerated(path, generated, true, true)
		}
	}
	if err := devLoop(ctx, devConfig{directory: *directory, packagePath: *packagePath, address: *address, watch: *watch, poll: *poll, buildTimeout: *buildTimeout, output: stdout}, deps); err != nil {
		_, _ = fmt.Fprintln(stderr, "local dev process stopped:", err)
		return 1
	}
	return 0
}

func validateDev(config devConfig) error {
	host, port, err := net.SplitHostPort(config.address)
	if err != nil {
		return errors.New("dev address must include a loopback IP and port")
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || number < 1 || number > 65535 {
		return errors.New("dev address must use a loopback IP and port 1 through 65535")
	}
	if config.poll < 50*time.Millisecond || config.buildTimeout <= 0 || config.buildTimeout > 10*time.Minute {
		return errors.New("dev polling must be at least 50ms and build timeout at most ten minutes")
	}
	return nil
}

var errDevBuild = errors.New("build or client generation failed; see the local diagnostics")

func devLoop(ctx context.Context, config devConfig, deps devDependencies) error {
	if err := validateDev(config); err != nil {
		return err
	}
	directory, err := filepath.Abs(config.directory)
	if err != nil {
		return err
	}
	if config.packagePath == "" {
		if _, err := os.Stat(filepath.Join(directory, "cmd", "invoices", "main.go")); err != nil {
			return errors.New("dev directory must contain the local invoice example")
		}
	} else {
		if !validPackageTarget(config.packagePath) {
			return errors.New("dev package must be one local main package")
		}
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err != nil {
			return errors.New("dev directory must contain go.mod")
		}
	}
	temporary, err := os.MkdirTemp("", "trest-dev-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	fingerprint, err := sourceFingerprint(directory)
	if err != nil {
		return err
	}
	var process devProcess
	defer func() {
		if process != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			_ = process.Stop(shutdown)
		}
	}()
	generation := 0
	emit := func(event string) {
		_ = writeJSON(config.output, struct {
			SchemaVersion string `json:"schema_version"`
			Event         string `json:"event"`
			Address       string `json:"address"`
			Generation    int    `json:"generation"`
		}{"trest.dev.v1", event, config.address, generation})
	}
	buildAndStart := func() error {
		generation++
		binary := filepath.Join(temporary, fmt.Sprintf("app-%d", generation))
		buildCtx, cancel := context.WithTimeout(ctx, config.buildTimeout)
		emit("building")
		err := deps.build(buildCtx, directory, binary)
		if err == nil && deps.afterBuild != nil {
			err = deps.afterBuild(buildCtx, binary, directory)
		}
		cancel()
		if err != nil {
			emit("build_failed")
			return errDevBuild
		}
		if process != nil {
			shutdown, stop := context.WithTimeout(context.Background(), 6*time.Second)
			err := process.Stop(shutdown)
			stop()
			if err != nil {
				return err
			}
		}
		process, err = deps.start(binary, config.address, config.output)
		if err != nil {
			return err
		}
		readyCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		if err := deps.ready(readyCtx, config.address, devInstance(binary)); err != nil {
			return errors.New("local app did not become ready; check its readiness route and instance marker")
		}
		emit("ready")
		return nil
	}
	if err := buildAndStart(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	ticker := time.NewTicker(config.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			emit("stopping")
			return nil
		case <-process.Done():
			if err := process.Err(); err != nil {
				return errors.New("local app exited unexpectedly")
			}
			return nil
		case <-ticker.C:
			if !config.watch {
				continue
			}
			next, err := sourceFingerprint(directory)
			if err != nil {
				return err
			}
			if next == fingerprint {
				continue
			}
			fingerprint = next
			if err := buildAndStart(); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// A failed compile keeps the previous process alive. A failed
				// replacement start/readiness cannot be silently called ready.
				if process == nil || !errors.Is(err, errDevBuild) {
					return err
				}
				select {
				case <-process.Done():
					return err
				default:
				}
			}
		}
	}
}

func sourceFingerprint(directory string) (string, error) {
	hash := sha256.New()
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != directory && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if filepath.Ext(path) != ".go" && entry.Name() != "go.mod" && entry.Name() != "go.sum" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(hash, "%s\x00%d\x00%d\n", path, info.Size(), info.ModTime().UnixNano())
		return err
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func devEnvironment() []string {
	keys := []string{"PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOTOOLCHAIN", "GOROOT"}
	result := []string{"TREST_DEMO_TOKEN=local-dev-fixture-token"}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			result = append(result, key+"="+value)
		}
	}
	return result
}

func buildProject(ctx context.Context, directory, binary, target string, diagnostics io.Writer) error {
	return buildTarget(ctx, directory, binary, target, projectEnvironment(""), diagnostics)
}
func buildTarget(ctx context.Context, directory, binary, target string, environment []string, diagnostics io.Writer) error {
	if !validPackageTarget(target) {
		return errors.New("invalid local package target")
	}
	command := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", binary, target)
	command.Dir = directory
	command.Env = environment
	command.Stdout = diagnostics
	command.Stderr = diagnostics
	command.WaitDelay = time.Second
	configureBuildCancellation(command)
	return command.Run()
}

type childProcess struct {
	command *exec.Cmd
	done    chan struct{}
	err     error
}

func (p *childProcess) Done() <-chan struct{} { return p.done }
func (p *childProcess) Err() error            { <-p.done; return p.err }
func (p *childProcess) Stop(ctx context.Context) error {
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := p.command.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = p.command.Process.Kill()
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		<-p.done
		return nil
	}
}

func devInstance(binary string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(binary))) }
func startTarget(binary, address, directory string, environment []string, diagnostics io.Writer) (devProcess, error) {
	command := exec.Command(binary, "-addr", address)
	command.Dir = directory
	command.Env = setDevInstance(environment, devInstance(binary))
	// Application output stays on stderr, separate from lifecycle JSON on stdout.
	command.Stdout = diagnostics
	command.Stderr = diagnostics
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &childProcess{command: command, done: make(chan struct{})}
	go func() { process.err = command.Wait(); close(process.done) }()
	return process, nil
}

func waitReadyPath(ctx context.Context, address, instance, path string, fixture bool) error {
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 250 * time.Millisecond, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return waitReadyPathWithClient(ctx, address, instance, path, fixture, client)
}

func waitReadyWithClient(ctx context.Context, address, instance string, client *http.Client) error {
	return waitReadyPathWithClient(ctx, address, instance, "/readyz", true, client)
}
func waitReadyPathWithClient(ctx context.Context, address, instance, path string, fixture bool, client *http.Client) error {
	if !validReadyPath(path) {
		return errors.New("invalid readiness path")
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+path, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(req)
		if err == nil {
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 256))
			closeErr := response.Body.Close()
			if readErr == nil && closeErr == nil && response.StatusCode == http.StatusOK && response.Header.Get("X-Trest-Dev-Instance") == instance && (!fixture || strings.TrimSpace(string(data)) == `{"ready":true}`) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func validReadyPath(path string) bool {
	return strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//") && !strings.ContainsAny(path, "?#\r\n\t ")
}
func projectEnvironment(instance string) []string { return setDevInstance(os.Environ(), instance) }
func setDevInstance(environment []string, instance string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "TREST_DEV_INSTANCE=") {
			result = append(result, entry)
		}
	}
	if instance != "" {
		result = append(result, "TREST_DEV_INSTANCE="+instance)
	}
	return result
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}
