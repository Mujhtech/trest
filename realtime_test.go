package trest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/operation"
)

type eventData struct {
	Count int `json:"count" min:"1"`
}

func TestTypedEventsEnforceAuthorizationAndContract(t *testing.T) {
	var handlers, producers int
	app, err := trest.New(auth(), trest.Group("", trest.RequireAuth(),
		trest.Get("/events", func(context.Context, trest.Empty) (trest.Events[eventData], error) {
			handlers++
			return trest.EventStream(func(_ context.Context, w *trest.JSONEventWriter[eventData]) error {
				producers++
				if err := w.Heartbeat(); err != nil {
					return err
				}
				return w.Write(trest.JSONEvent[eventData]{ID: "7", Event: "updated", Data: eventData{Count: 2}, Retry: 2 * time.Second})
			}), nil
		}).Name("events"),
	))
	if err != nil {
		t.Fatal(err)
	}
	if w := request(t, app, "GET", "/events", "", ""); w.Code != 401 || handlers != 0 || producers != 0 {
		t.Fatalf("unauthenticated status=%d handlers=%d producers=%d", w.Code, handlers, producers)
	}
	w := request(t, app, "GET", "/events", "", "acme")
	want := ": keep-alive\n\nid: 7\nevent: updated\nretry: 2000\ndata: {\"count\":2}\n\n"
	if w.Code != 200 || w.Body.String() != want || !w.Flushed || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response=%d headers=%v body=%q", w.Code, w.Header(), w.Body.String())
	}
	if w := request(t, app, "HEAD", "/events", "", "acme"); w.Code != 200 || w.Body.Len() != 0 || producers != 1 {
		t.Fatalf("HEAD status=%d body=%q producers=%d", w.Code, w.Body.String(), producers)
	}
	routes := app.Routes()
	if !routes[0].NativeOnly || routes[0].Stream == nil || routes[0].Stream.Format != "sse" {
		t.Fatalf("routes=%+v", routes)
	}
	if _, err := app.Registry().InvokeJSON(context.Background(), routes[0].OperationID, json.RawMessage(`{}`)); operation.CodeOf(err) != operation.InvalidInput {
		t.Fatalf("JSON bridge: %v", err)
	}
	first, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	routes[0].Stream.Schema[0] = 'x'
	routes[0].Stream.Format = "changed"
	second, err := app.OpenAPI()
	if err != nil || string(first) != string(second) {
		t.Fatalf("metadata is mutable: %v", err)
	}
	var contract map[string]any
	if err := json.Unmarshal(first, &contract); err != nil {
		t.Fatal(err)
	}
	stream := contract["paths"].(map[string]any)["/events"].(map[string]any)["get"].(map[string]any)["x-trest-stream"].(map[string]any)
	if stream["format"] != "sse" || !strings.Contains(string(first), `"minimum": 1`) {
		t.Fatalf("stream contract=%v", stream)
	}
}

func TestTypedEventsValidateBeforeWritingAndAbortPartialStreams(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "partial"}[started], func(t *testing.T) {
			var observed trest.Event
			app, err := trest.New(trest.AfterResponse(func(e trest.Event) { observed = e }),
				trest.Get("/events", func(context.Context, trest.Empty) (trest.Events[eventData], error) {
					return trest.EventStream(func(_ context.Context, w *trest.JSONEventWriter[eventData]) error {
						if started {
							if err := w.Send("1", "", eventData{Count: 1}); err != nil {
								return err
							}
						}
						return w.Send("2", "", eventData{Count: 0})
					}), nil
				}))
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			p := protocolPanic(func() { app.ServeHTTP(w, httptest.NewRequest("GET", "/events", nil)) })
			if observed.Code != operation.Internal {
				t.Fatalf("event=%+v", observed)
			}
			if started {
				if p != http.ErrAbortHandler || w.Code != 200 || w.Body.String() != "id: 1\ndata: {\"count\":1}\n\n" {
					t.Fatalf("panic=%v status=%d body=%q", p, w.Code, w.Body.String())
				}
			} else if p != nil || w.Code != 500 || strings.Contains(w.Body.String(), "schema") {
				t.Fatalf("panic=%v status=%d body=%s", p, w.Code, w.Body.String())
			}
		})
	}
}

func TestTypedEventsCancelBeforeWriteAndRejectInvalidConstruction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var writeErr error
	app, err := trest.New(trest.Get("/", func(context.Context, trest.Empty) (trest.Events[eventData], error) {
		return trest.EventStream(func(_ context.Context, w *trest.JSONEventWriter[eventData]) error {
			cancel()
			writeErr = w.Send("", "", eventData{Count: 1})
			return writeErr
		}), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx))
	if !errors.Is(writeErr, context.Canceled) || strings.Contains(w.Body.String(), "data:") {
		t.Fatalf("write=%v body=%s", writeErr, w.Body.String())
	}
	base := trest.Get("/", trest.Const(trest.EventStream[eventData](nil)))
	for _, route := range []trest.Component{base.Status(201), base.Statuses(200), base.Produces("text/plain")} {
		if _, err := trest.New(route); err == nil {
			t.Fatal("accepted conflicting event contract")
		}
	}
	nilApp, err := trest.New(base)
	if err != nil {
		t.Fatal(err)
	}
	if w := request(t, nilApp, "GET", "/", "", ""); w.Code != 500 {
		t.Fatalf("nil callback status=%d", w.Code)
	}
}

func TestEventFrameSizeIncludesFraming(t *testing.T) {
	app, err := trest.New(trest.Get("/", trest.Const(trest.SSE(func(_ context.Context, w *trest.EventWriter) error {
		return w.Send(trest.Message{Data: strings.Repeat("\n", 200000)})
	}))))
	if err != nil {
		t.Fatal(err)
	}
	if w := request(t, app, "GET", "/", "", ""); w.Code != 500 || strings.Contains(w.Body.String(), "data:") {
		t.Fatalf("status=%d body size=%d", w.Code, w.Body.Len())
	}
}

type linkedEvent struct {
	Value string       `json:"value"`
	Next  *linkedEvent `json:"next,omitempty"`
}

func TestRealtimeRecursiveSchemasAndMetadataIsolation(t *testing.T) {
	raw := trest.WebSocketMessages[linkedEvent, eventData](trest.Raw("GET", "/socket", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(426) }))).Name("socket")
	app, err := trest.New(raw, trest.Get("/events", trest.Const(trest.EventStream[linkedEvent](nil))).Name("events"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(first), `#/$defs/`) || !strings.Contains(string(first), `#/components/schemas/`) || !strings.Contains(string(first), `"x-trest-websocket"`) {
		t.Fatalf("invalid refs: %s", first)
	}
	for _, route := range app.Routes() {
		if route.WebSocket != nil {
			route.WebSocket.Input[0] = 'x'
			route.WebSocket.Output[0] = 'x'
		}
	}
	second, err := app.OpenAPI()
	if err != nil || string(first) != string(second) {
		t.Fatalf("mutable websocket schema: %v", err)
	}
}

func TestResponseWrapperPointersAndNestedStreamsFailConstruction(t *testing.T) {
	response := trest.JSON(200, eventData{Count: 1})
	events := trest.EventStream[eventData](nil)
	reply := trest.Text("hello")
	for _, route := range []trest.Component{
		trest.Get("/", trest.Const(&response)),
		trest.Get("/", trest.Const(&events)),
		trest.Get("/", trest.Const(&reply)),
		trest.Get("/", trest.Const(trest.JSON(200, events))),
		trest.Get("/", trest.Const(trest.JSON(200, &reply))),
	} {
		var err error
		p := protocolPanic(func() { _, err = trest.New(route) })
		var diagnostic *trest.ConstructionError
		if p != nil || !errors.As(err, &diagnostic) {
			t.Fatalf("panic=%v error=%v", p, err)
		}
	}
}

func TestRealtimeAndHTTPComponentNamespacesCannotCollide(t *testing.T) {
	app, err := trest.New(
		trest.WebSocketMessages[linkedEvent, linkedEvent](trest.Raw("GET", "/chat", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).Name("chat")),
		trest.Post("/messages", func(context.Context, linkedEvent) (trest.Empty, error) { return trest.Empty{}, nil }).Name("chatMessage"),
		trest.Get("/events", trest.Const(trest.EventStream[linkedEvent](nil))).Name("chatMessageInput"),
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.OpenAPI()
	if err != nil || string(first) != string(second) {
		t.Fatalf("unstable contract: %v", err)
	}
}
