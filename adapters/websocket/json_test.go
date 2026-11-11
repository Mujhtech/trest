package trestws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mujhtech/trest"
)

type echoMessage struct {
	Text  string       `json:"text" minLength:"2"`
	Count int          `json:"count" min:"1" max:"5"`
	Next  *echoMessage `json:"next,omitempty"`
}

func TestTypedWebSocketMetadata(t *testing.T) {
	t.Parallel()
	component, err := NewJSON[echoMessage, echoMessage]("/ws", Options{Name: "typedEcho", Public: true, AllowMissingOrigin: true}, func(context.Context, *JSONConn[echoMessage, echoMessage]) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	app, err := trest.New(component)
	if err != nil {
		t.Fatal(err)
	}
	document, err := app.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(document, &spec); err != nil {
		t.Fatal(err)
	}
	var messages map[string]json.RawMessage
	if err := json.Unmarshal(spec.Paths["/ws"]["get"]["x-trest-websocket"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages["input"]) == 0 || len(messages["output"]) == 0 || !bytes.Contains(document, []byte(`"minLength"`)) || bytes.Contains(document, []byte(`"$ref": "#/$defs/`)) {
		t.Fatalf("typed schemas missing or not rebased: %s", document)
	}
	if len(app.Registry().Operations()) != 0 {
		t.Fatal("WebSocket message route entered JSON job/tool registry")
	}
}

func TestNewJSONRejectsInvalidDeclarations(t *testing.T) {
	t.Parallel()
	options := Options{Public: true, AllowMissingOrigin: true}
	if _, err := NewJSON[echoMessage, echoMessage]("/ws", options, nil); err == nil {
		t.Fatal("nil handler accepted")
	}
	if _, err := NewJSON[any, echoMessage]("/ws", options, func(context.Context, *JSONConn[any, echoMessage]) error { return nil }); err == nil {
		t.Fatal("untyped input accepted")
	}
	if _, err := NewJSON[echoMessage, chan string]("/ws", options, func(context.Context, *JSONConn[echoMessage, chan string]) error { return nil }); err == nil {
		t.Fatal("unsupported output accepted")
	}
	if _, err := NewJSON[echoMessage, echoMessage]("/ws", Options{}, func(context.Context, *JSONConn[echoMessage, echoMessage]) error { return nil }); err == nil {
		t.Fatal("implicit public access accepted")
	}
}

func TestTypedConnectionZeroValueAndOutputValidation(t *testing.T) {
	t.Parallel()
	var empty JSONConn[echoMessage, echoMessage]
	if _, err := empty.Read(context.Background()); err == nil {
		t.Fatal("uninitialized reader accepted")
	}
	if err := empty.Write(context.Background(), echoMessage{}); err == nil {
		t.Fatal("uninitialized writer accepted")
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	validator, err := compileMessage(reflect.TypeFor[echoMessage]())
	if err != nil {
		t.Fatal(err)
	}
	conn := &JSONConn[echoMessage, echoMessage]{conn: &Conn{maxBytes: 1024}, input: validator, output: validator}
	var rejected *MessageError
	if err := conn.Write(context.Background(), echoMessage{Text: "ok", Count: 0}); !errors.As(err, &rejected) || rejected.Direction != "output" || len(rejected.Fields) != 1 || rejected.Fields[0].Code != "range" {
		t.Fatalf("invalid output reached transport: %v", err)
	}
}

func TestClientOptionsFailBeforeDial(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		options ClientOptions
	}{
		{"message size", ClientOptions{MaxMessageBytes: -1}},
		{"dial timeout", ClientOptions{DialTimeout: -time.Second}},
		{"read timeout", ClientOptions{ReadTimeout: -time.Second}},
		{"write timeout", ClientOptions{WriteTimeout: -time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, response, err := DialJSONTransport(context.Background(), "ws://invalid.example", tc.options)
			if err == nil || conn != nil || response != nil {
				t.Fatal("invalid client configuration reached dial")
			}
		})
	}
}

var _ interface {
	Read(context.Context) ([]byte, error)
	Write(context.Context, []byte) error
	Close() error
} = (*JSONTransport)(nil)
