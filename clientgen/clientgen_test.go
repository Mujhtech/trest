package clientgen_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mujhtech/trest/clientgen"
)

func contract(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func TestGenerationIsDeterministic(t *testing.T) {
	data := contract(t)
	for _, test := range []struct {
		name     string
		generate func([]byte) ([]byte, error)
	}{{"TypeScript", clientgen.TypeScript}, {"Go", func(data []byte) ([]byte, error) { return clientgen.Go(data, "client") }}} {
		t.Run(test.name, func(t *testing.T) {
			first, err := test.generate(data)
			if err != nil {
				t.Fatal(err)
			}
			second, err := test.generate(data)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("generation changed without a contract change")
			}
			if !bytes.Contains(first, []byte("getInvoice")) && !bytes.Contains(first, []byte("GetInvoice")) {
				t.Fatal("missing operation")
			}
			if !bytes.Contains(first, []byte("Skipped socket: use a native WebSocket client")) {
				t.Fatal("missing explanation for skipped WebSocket operation")
			}
			if bytes.Contains(first, []byte("SocketInput")) {
				t.Fatal("WebSocket operation generated an HTTP client method")
			}
		})
	}
}
func TestUnsupportedContractsFailBeforeGenerating(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unsupported_version", func(d map[string]any) { d["openapi"] = "3.0.0" }},
		{"operation_injection", func(d map[string]any) {
			d["paths"].(map[string]any)["/events"].(map[string]any)["get"].(map[string]any)["operationId"] = "evil\";alert(1)//"
		}},
		{"external_reference", func(d map[string]any) {
			d["components"].(map[string]any)["schemas"].(map[string]any)["Invoice"] = map[string]any{"$ref": "https://example.com/schema"}
		}},
		{"unknown_union", func(d map[string]any) {
			d["components"].(map[string]any)["schemas"].(map[string]any)["Invoice"] = map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "number"}}}
		}},
		{"recursive_value_without_indirection", func(d map[string]any) {
			d["components"].(map[string]any)["schemas"].(map[string]any)["Invoice"] = map[string]any{
				"type": "object", "properties": map[string]any{"child": map[string]any{"$ref": "#/components/schemas/Invoice"}},
				"required": []string{"child"},
			}
		}},
		{"recursive_alias_without_indirection", func(d map[string]any) {
			d["components"].(map[string]any)["schemas"].(map[string]any)["Invoice"] = map[string]any{"$ref": "#/components/schemas/Invoice"}
		}},
		{"missing_path_declaration", func(d map[string]any) {
			delete(d["paths"].(map[string]any)["/invoices/{id}"].(map[string]any), "parameters")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var d map[string]any
			if err := json.Unmarshal(contract(t), &d); err != nil {
				t.Fatal(err)
			}
			test.mutate(d)
			data, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := clientgen.Go(data, "client"); err == nil {
				t.Fatal("Go accepted invalid contract")
			}
			if _, err := clientgen.TypeScript(data); err == nil {
				t.Fatal("TS accepted invalid contract")
			}
		})
	}
}

func TestRecursiveNamedSchemasGenerate(t *testing.T) {
	data := []byte(`{
		"openapi":"3.1.1","paths":{},"components":{"schemas":{
			"Node":{"type":"object","properties":{
				"label":{"type":"string"},
				"children":{"type":"array","items":{"$ref":"#/components/schemas/Node"}},
				"parent":{"anyOf":[{"$ref":"#/components/schemas/Node"},{"type":"null"}]},
				"links":{"type":"object","additionalProperties":{"$ref":"#/components/schemas/Node"}}
			},"required":["label","children","parent","links"]}
		}}
	}`)
	for _, test := range []struct {
		name     string
		generate func([]byte) ([]byte, error)
	}{
		{name: "Go", generate: func(data []byte) ([]byte, error) { return clientgen.Go(data, "client") }},
		{name: "TypeScript", generate: clientgen.TypeScript},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.generate(data); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestSchemaNamesAndPropertiesAreEscaped(t *testing.T) {
	data := []byte(`{"openapi":"3.1.1","paths":{},"components":{"schemas":{"Safe":{"type":"object","properties":{"strange-tag":{"type":"string"}},"additionalProperties":false}}}}`)
	out, err := clientgen.Go(data, "client")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "StrangeTag") {
		t.Fatal("property name was lost")
	}
	if _, err := clientgen.Go(data, "foo; import os"); err == nil {
		t.Fatal("invalid package name accepted")
	}
}

func TestInvalidRealtimeMetadataFailsGeneration(t *testing.T) {
	for _, tc := range []struct{ name, operation string }{
		{"unknown stream format", `{"operationId":"events","x-trest-stream":{"format":"ndjson","schema":{"type":"string"}},"responses":{"200":{"content":{"text/event-stream":{}}}}}`},
		{"missing event schema", `{"operationId":"events","x-trest-stream":{"format":"sse"},"responses":{"200":{"content":{"text/event-stream":{}}}}}`},
		{"event content mismatch", `{"operationId":"events","x-trest-stream":{"format":"sse","schema":{"type":"string"}},"responses":{"200":{"content":{"application/json":{"schema":{"type":"string"}}}}}}`},
		{"socket missing protocol", `{"operationId":"chat","x-trest-websocket":{"input":{"type":"string"},"output":{"type":"string"}},"responses":{"200":{}}}`},
		{"socket missing output", `{"operationId":"chat","x-trest-protocol":"websocket","x-trest-websocket":{"input":{"type":"string"}},"responses":{"101":{}}}`},
		{"socket HTTP body", `{"operationId":"chat","x-trest-protocol":"websocket","x-trest-websocket":{"input":{"type":"string"},"output":{"type":"string"}},"requestBody":{"content":{"application/json":{"schema":{"type":"string"}}}},"responses":{"101":{}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contract := []byte(`{"openapi":"3.1.0","paths":{"/realtime":{"get":` + tc.operation + `}}}`)
			if _, err := clientgen.Go(contract, "client"); err == nil {
				t.Fatal("Go accepted invalid realtime metadata")
			}
			if _, err := clientgen.TypeScript(contract); err == nil {
				t.Fatal("TypeScript accepted invalid realtime metadata")
			}
		})
	}
}
