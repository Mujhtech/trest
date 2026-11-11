package trest_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/operation"
)

// protocolResponse records informational responses separately from the final
// status, which httptest.ResponseRecorder does not model. FlushError emulates a
// transport failure without opening a listener.
type protocolResponse struct {
	header    http.Header
	codes     []int
	status    int
	body      bytes.Buffer
	flushErr  error
	flushes   int
	deadlines int
}

func (p *protocolResponse) Header() http.Header {
	if p.header == nil {
		p.header = make(http.Header)
	}
	return p.header
}
func (p *protocolResponse) WriteHeader(code int) {
	p.codes = append(p.codes, code)
	if code >= 200 || code == 101 {
		p.status = code
	}
}
func (p *protocolResponse) Write(data []byte) (int, error) {
	if p.status == 0 {
		p.WriteHeader(200)
	}
	return p.body.Write(data)
}
func (p *protocolResponse) FlushError() error                { p.flushes++; return p.flushErr }
func (p *protocolResponse) SetWriteDeadline(time.Time) error { p.deadlines++; return nil }

func TestRawEarlyHintsPreserveFinalStatus(t *testing.T) {
	var observed []trest.Event
	app, err := trest.New(
		trest.AfterResponse(func(event trest.Event) { observed = append(observed, event) }),
		trest.Raw("GET", "/resource", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Add("Link", "</style.css>; rel=preload; as=style")
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, "created")
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := &protocolResponse{}
	app.ServeHTTP(response, httptest.NewRequest("GET", "/resource", nil))
	if !reflect.DeepEqual(response.codes, []int{103, 201}) || response.status != 201 || response.body.String() != "created" {
		t.Fatalf("codes=%v status=%d body=%q", response.codes, response.status, response.body.String())
	}
	if len(observed) != 1 || observed[0].Status != 201 || observed[0].Code != "" {
		t.Fatalf("events=%+v", observed)
	}
}

func TestStreamFlushFailurePropagatesAndAbortsAfterObservation(t *testing.T) {
	transportError := errors.New("test transport failed")
	var callbackError error
	var observed []trest.Event
	var sequence []string
	app, err := trest.New(
		trest.AfterResponse(func(event trest.Event) { observed = append(observed, event); sequence = append(sequence, "observed") }),
		trest.Get("/stream", func(context.Context, trest.Empty) (trest.Reply, error) {
			return trest.Stream("text/plain", func(_ context.Context, w io.Writer) error {
				_, callbackError = io.WriteString(w, "partial")
				return callbackError
			}), nil
		}).Produces("text/plain"),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := &protocolResponse{flushErr: transportError}
	recovered := protocolPanic(func() { app.ServeHTTP(response, httptest.NewRequest("GET", "/stream", nil)) })
	sequence = append(sequence, "returned")
	if recovered != http.ErrAbortHandler {
		t.Fatalf("panic=%v; want ErrAbortHandler", recovered)
	}
	if !errors.Is(callbackError, transportError) {
		t.Fatalf("callback error=%v", callbackError)
	}
	if response.flushes != 1 || response.deadlines != 1 || response.body.String() != "partial" {
		t.Fatalf("flushes=%d deadlines=%d body=%q", response.flushes, response.deadlines, response.body.String())
	}
	if !reflect.DeepEqual(sequence, []string{"observed", "returned"}) || len(observed) != 1 || observed[0].Status != 200 || observed[0].Code != operation.Internal {
		t.Fatalf("sequence=%v events=%+v", sequence, observed)
	}
}

func TestNativeCallbackStatusChangesFailBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response func(*int) trest.Reply
	}{
		{"file", func(_ *int) trest.Reply {
			return trest.File(fstest.MapFS{"private.txt": {Data: []byte("private content")}}, "private.txt").WithStatus(201)
		}},
		{"stream", func(calls *int) trest.Reply {
			return trest.Stream("text/plain", func(_ context.Context, w io.Writer) error {
				*calls++
				_, err := io.WriteString(w, "private content")
				return err
			}).WithStatus(201)
		}},
		{"redirect", func(_ *int) trest.Reply { return trest.Redirect("/elsewhere", 302).WithStatus(307) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			app, err := trest.New(trest.Get("/", trest.Const(tc.response(&calls))))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			app.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
			if response.Code != 500 || calls != 0 || response.Header().Get("Location") != "" {
				t.Fatalf("status=%d callbacks=%d headers=%v", response.Code, calls, response.Header())
			}
			if bytes.Contains(response.Body.Bytes(), []byte("private content")) || bytes.Contains(response.Body.Bytes(), []byte("constructor")) {
				t.Fatalf("private result leaked: %s", response.Body.String())
			}
		})
	}
	app, err := trest.New(trest.Get("/", trest.Const(trest.Text("created").WithStatus(201))))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest("GET", "/", nil))
	if response.Code != 201 || response.Body.String() != "created" {
		t.Fatalf("buffered response: %d %q", response.Code, response.Body.String())
	}
}

func TestStreamingPanicAbortsAfterResponseHook(t *testing.T) {
	var event trest.Event
	hooks := 0
	app, err := trest.New(
		trest.AfterResponse(func(observed trest.Event) { event = observed; hooks++ }),
		trest.Get("/stream", func(context.Context, trest.Empty) (trest.Reply, error) {
			return trest.Stream("text/plain", func(_ context.Context, w io.Writer) error {
				if _, err := io.WriteString(w, "first chunk"); err != nil {
					return err
				}
				panic("private application state")
			}), nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	response := &protocolResponse{}
	recovered := protocolPanic(func() { app.ServeHTTP(response, httptest.NewRequest("GET", "/stream", nil)) })
	if recovered != http.ErrAbortHandler || hooks != 1 || event.Status != 200 || event.Code != operation.Internal {
		t.Fatalf("panic=%v hooks=%d event=%+v", recovered, hooks, event)
	}
	if response.body.String() != "first chunk" {
		t.Fatalf("unexpected body appended after panic: %q", response.body.String())
	}
}
func protocolPanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

type protocolListener struct {
	closed chan struct{}
	once   sync.Once
}

func (l *protocolListener) Accept() (net.Conn, error) { <-l.closed; return nil, net.ErrClosed }
func (l *protocolListener) Close() error              { l.once.Do(func() { close(l.closed) }); return nil }
func (l *protocolListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func TestRunCanRetryAfterStartupFailureWithoutLeakingOwnership(t *testing.T) {
	var attempts, active, stops atomic.Int32
	running := make(chan struct{})
	app, err := trest.New(trest.Lifecycle(func(context.Context) error {
		if attempts.Add(1) == 1 {
			return errors.New("startup failed")
		}
		active.Add(1)
		close(running)
		return nil
	}, func(context.Context) error { active.Add(-1); stops.Add(1); return nil }))
	if err != nil {
		t.Fatal(err)
	}
	first := &protocolListener{closed: make(chan struct{})}
	if err = app.Run(context.Background(), first); err == nil {
		t.Fatal("startup failure was lost")
	}
	select {
	case <-first.closed:
	default:
		t.Fatal("failed startup retained listener")
	}
	if active.Load() != 0 || stops.Load() != 0 {
		t.Fatalf("failed startup cleanup affected unacquired resource: active=%d stops=%d", active.Load(), stops.Load())
	}
	second := &protocolListener{closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- app.Run(ctx, second) }()
	select {
	case <-running:
	case <-time.After(time.Second):
		t.Fatal("retry could not start")
	}
	cancel()
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry did not shut down")
	}
	if active.Load() != 0 || stops.Load() != 1 {
		t.Fatalf("remaining ownership: active=%d stops=%d", active.Load(), stops.Load())
	}
	select {
	case <-second.closed:
	default:
		t.Fatal("shutdown retained listener")
	}
}
