package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mujhtech/trest/clientgen"
)

func runNew(args []string, stdout, stderr io.Writer) int {
	// Accept both "new app --module ..." and the stdlib flag-before-argument form.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		args = append(append([]string(nil), args[1:]...), args[0])
	}
	set := flags("new", stderr)
	module := set.String("module", "", "Go module path (defaults to example.local/<directory>)")
	trestDir := set.String("trest-dir", "", "local github.com/mujhtech/trest source directory")
	if set.Parse(args) != nil || set.NArg() != 1 {
		return 2
	}
	destination, err := filepath.Abs(set.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if *module == "" {
		name := regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(filepath.Base(destination)), "-")
		name = strings.Trim(name, "-")
		if name == "" {
			name = "myapp"
		}
		*module = "example.local/" + name
	}
	if !validModule(*module) {
		_, _ = fmt.Fprintln(stderr, "module must be a valid slash-separated Go module path")
		return 2
	}
	source, err := findTrestSource(*trestDir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	files, err := scaffoldFiles(*module, source)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if err := os.Mkdir(destination, 0o755); err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot create new app; choose a directory that does not exist:", err)
		return 1
	}
	for _, name := range sortedFileNames(files) {
		path := filepath.Join(destination, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			_, _ = fmt.Fprintln(stderr, "could not finish app; created files were preserved:", err)
			return 1
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "could not finish app; existing files were preserved:", err)
			return 1
		}
		_, writeErr := file.Write(files[name])
		if err := errors.Join(writeErr, file.Close()); err != nil {
			_, _ = fmt.Fprintln(stderr, "could not finish app; created files were preserved:", err)
			return 1
		}
	}
	_, _ = fmt.Fprintf(stdout, "Created %s\n\ncd %s\ngo test ./...\ngo run ./cmd/server\n\nThe local Trest module is linked from %s.\n", destination, strconv.Quote(destination), source)
	return 0
}

func sortedFileNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func validModule(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, "-") {
			return false
		}
		for _, r := range part {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_", r) {
				continue
			}
			return false
		}
	}
	return true
}
func findTrestSource(explicit string) (string, error) {
	valid := func(path string) (string, bool) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", false
		}
		data, err := os.ReadFile(filepath.Join(absolute, "go.mod"))
		if err != nil {
			return "", false
		}
		if strings.Contains(string(data), "module github.com/mujhtech/trest\n") {
			return absolute, true
		}
		return "", false
	}
	if explicit != "" {
		if path, ok := valid(explicit); ok {
			return path, nil
		}
		return "", errors.New("--trest-dir must contain the local github.com/mujhtech/trest module")
	}
	if directory, err := os.Getwd(); err == nil {
		for {
			if path, ok := valid(directory); ok {
				return path, nil
			}
			parent := filepath.Dir(directory)
			if parent == directory {
				break
			}
			directory = parent
		}
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		if path, ok := valid(filepath.Join(filepath.Dir(file), "..", "..")); ok {
			return path, nil
		}
	}
	return "", errors.New("cannot locate the github.com/mujhtech/trest module; pass --trest-dir with its source directory")
}

func scaffoldFiles(module, source string) (map[string][]byte, error) {
	mainCode, err := format.Source([]byte(strings.ReplaceAll(scaffoldMain, "__BT__", "`")))
	if err != nil {
		return nil, err
	}
	testCode, err := format.Source([]byte(scaffoldTest))
	if err != nil {
		return nil, err
	}
	readme := fmt.Sprintf("# %s\n\nA typed Go application built with Trest.\n\n```sh\ngo test ./...\ngo run ./cmd/server\n```\n\nOpen http://127.0.0.1:8080/docs for interactive API documentation. Try `/greet/Ada`.\n\nThe generated project uses a local `replace` for Trest at %s for development against this checkout. Update that replacement when moving the project.\n\nWith the `trest` command installed or built from that source:\n\n```sh\ntrest dev --dir . --watch\ntrest inspect --dir . --json\ntrest check --dir . --run-go-checks\ntrest contract export --dir . --out openapi.json\ntrest contract export --dir . --baseline --out openapi.baseline.json\ntrest check --dir . --baseline openapi.baseline.json\ntrest client --spec openapi.json --lang ts --out client.ts\n# Regenerate a marked client on every successful rebuild:\ntrest dev --dir . --watch --client ts --client-out client.ts\n```\n\nFor a Go client use `--lang go --package client --out client/client.go`; create the output directory first. File replacement requires `--overwrite` on explicit client/contract exports. The development watcher only replaces files carrying Trest's generated-file marker.\n\n`dev` builds and runs your local application and inherits your environment. It expects the `-addr`, `-openapi`, and `-manifest` flags supplied by this starter. Readiness uses `/readyz` and a per-process marker; use `--ready-path` if you move that route. The server is bound to loopback by default.\n", module, strconv.Quote(source))
	return map[string][]byte{
		"go.mod":             []byte("module " + module + "\n\ngo 1.23.0\n\nrequire github.com/mujhtech/trest v0.0.0\n\nreplace github.com/mujhtech/trest => " + strconv.Quote(filepath.ToSlash(source)) + "\n"),
		"cmd/server/main.go": mainCode, "cmd/server/main_test.go": testCode, "README.md": []byte(readme), ".gitignore": []byte("/server\n*.log\n"),
	}, nil
}

const scaffoldMain = `package main

import (
 "context"
 "encoding/json"
 "flag"
 "fmt"
 "log"
 "net/http"
 "os"
 "os/signal"
 "syscall"

 "github.com/mujhtech/trest"
)

type greetInput struct { Name string __BT__path:"name" minLength:"2"__BT__ }
type greeting struct { Message string __BT__json:"message"__BT__ }
type readiness struct { Ready bool __BT__json:"ready"__BT__ }

func application() (*trest.App,error) {
 return trest.New(
  trest.Info("My Trest app","0.1.0"),
  trest.Get("/",trest.Const("Hello from Trest")).Name("hello"),
  trest.Get("/greet/{name}",func(_ context.Context,in greetInput)(greeting,error){return greeting{Message:"Hello, "+in.Name},nil}).Name("greet"),
  trest.Group("",
   trest.Use(func(next http.Handler)http.Handler{return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){if marker:=os.Getenv("TREST_DEV_INSTANCE");marker!=""{w.Header().Set("X-Trest-Dev-Instance",marker)};next.ServeHTTP(w,r)})}),
   trest.Get("/readyz",trest.Const(readiness{Ready:true})).Name("ready"),
  ),
  trest.Docs("/docs"),
 )
}

func main(){if err:=run();err!=nil{log.Fatal(err)}}
func run()error{
 address:=flag.String("addr","127.0.0.1:8080","listen address")
 openapi:=flag.Bool("openapi",false,"write OpenAPI and exit")
 manifest:=flag.Bool("manifest",false,"write application manifest and exit")
 flag.Parse()
 if flag.NArg()!=0||(*openapi&&*manifest){return fmt.Errorf("choose one export mode or a listen address")}
 app,err:=application();if err!=nil{return err}
 if *openapi{data,err:=app.OpenAPI();if err!=nil{return err};_,err=os.Stdout.Write(data);return err}
 if *manifest{return json.NewEncoder(os.Stdout).Encode(struct{SchemaVersion string __BT__json:"schema_version"__BT__;Routes []trest.RouteInfo __BT__json:"routes"__BT__}{"trest.app.v1",app.Routes()})}
 ctx,cancel:=signal.NotifyContext(context.Background(),os.Interrupt,syscall.SIGTERM);defer cancel()
 log.Printf("Trest listening on http://%s; docs at /docs",*address)
 return app.Listen(ctx,*address)
}
`
const scaffoldTest = `package main

import (
 "net/http/httptest"
 "strings"
 "testing"
)
func TestGreeting(t *testing.T){
 app,err:=application();if err!=nil{t.Fatal(err)}
 for _,tc:=range []struct{path string;status int;body string}{
  {"/greet/Ada",200,"Hello, Ada"},
  {"/greet/A",400,"invalid_input"},
 }{t.Run(tc.path,func(t *testing.T){response:=httptest.NewRecorder();app.ServeHTTP(response,httptest.NewRequest("GET",tc.path,nil));if response.Code!=tc.status||!strings.Contains(response.Body.String(),tc.body){t.Fatalf("response: %d %s",response.Code,response.Body.String())}})}
}
`

func runClient(args []string, stdout, stderr io.Writer) int {
	set := flags("client", stderr)
	spec := set.String("spec", "", "local OpenAPI 3.1 JSON file")
	language := set.String("lang", "ts", "client language: ts or go")
	packageName := set.String("package", "client", "generated Go package name")
	output := set.String("out", "-", "output file, or - for stdout")
	overwrite := set.Bool("overwrite", false, "explicitly replace an existing regular output file")
	if set.Parse(args) != nil || set.NArg() != 0 || *spec == "" || (*language != "ts" && *language != "go") {
		return 2
	}
	data, err := readBounded(*spec)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot read contract:", err)
		return 1
	}
	generated, err := generateClient(data, *language, *packageName)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot generate client:", err)
		return 1
	}
	if *output == "-" {
		if _, err := stdout.Write(generated); err != nil {
			return 1
		}
		return 0
	}
	if err := writeGenerated(*output, generated, *overwrite, false); err != nil {
		_, _ = fmt.Fprintln(stderr, "cannot write client:", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, "Generated", *output)
	return 0
}
func generateClient(data []byte, language, packageName string) ([]byte, error) {
	if language == "go" {
		return clientgen.Go(data, packageName)
	}
	if language == "ts" {
		return clientgen.TypeScript(data)
	}
	return nil, errors.New("client language must be ts or go")
}

func writeGenerated(path string, data []byte, overwrite, markedOnly bool) error {
	if len(data) > maxLocalFile {
		return errors.New("generated output exceeds size limit")
	}
	mode := os.FileMode(0o644)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("output must be a regular file, not a symlink or directory")
		}
		if !overwrite {
			return errors.New("output exists; pass --overwrite to replace it")
		}
		mode = info.Mode().Perm()
		existing, err := readBounded(path)
		if err != nil {
			return err
		}
		if markedOnly {
			if !bytes.HasPrefix(existing, []byte("// Code generated by Trest. DO NOT EDIT.")) {
				return errors.New("watcher only replaces Trest-generated client files")
			}
		}
		// Avoid touching unchanged generated Go files, which would otherwise cause
		// the source watcher to rebuild and regenerate indefinitely.
		if bytes.Equal(existing, data) {
			return nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".trest-generated-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if overwrite {
		return os.Rename(name, path)
	}
	// Linking a completed file gives exclusive, atomic creation without a window
	// where a partial generated file can be observed.
	return os.Link(name, path)
}

func validPackageTarget(target string) bool {
	if target == "." {
		return true
	}
	if !strings.HasPrefix(target, "./") || strings.ContainsAny(target, "\x00\r\n\t ") || strings.Contains(target, "...") {
		return false
	}
	for _, part := range strings.Split(target[2:], "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_", r) {
				continue
			}
			return false
		}
	}
	return true
}

func exportProject(ctx context.Context, directory, target, mode string, stderr io.Writer) ([]byte, error) {
	if !validPackageTarget(target) {
		return nil, errors.New("package must name one local main package, such as ./cmd/server")
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(absolute, "go.mod")); err != nil {
		return nil, errors.New("project directory must contain go.mod")
	}
	temporary, err := os.MkdirTemp("", "trest-export-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	binary := filepath.Join(temporary, "app")
	bounded, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := buildProject(bounded, absolute, binary, target, stderr); err != nil {
		return nil, err
	}
	return runExport(bounded, binary, absolute, mode, stderr)
}

type limitedOutput struct {
	bytes.Buffer
	maximum int
}

func (b *limitedOutput) Write(data []byte) (int, error) {
	if len(data) > b.maximum-b.Len() {
		return 0, errors.New("application export exceeds size limit")
	}
	return b.Buffer.Write(data)
}
func runExport(ctx context.Context, binary, directory, mode string, stderr io.Writer) ([]byte, error) {
	if mode != "openapi" && mode != "manifest" {
		return nil, errors.New("unsupported export mode")
	}
	command := exec.CommandContext(ctx, binary, "-"+mode)
	command.Dir = directory
	command.Env = projectEnvironment("")
	command.Stderr = stderr
	output := &limitedOutput{maximum: maxLocalFile}
	command.Stdout = output
	command.WaitDelay = time.Second
	configureBuildCancellation(command)
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("application %s export failed: %w", mode, err)
	}
	if !json.Valid(output.Bytes()) {
		return nil, errors.New("application export is not valid JSON")
	}
	return append([]byte(nil), output.Bytes()...), nil
}
