// Package contract exports deterministic OpenAPI descriptions of explicitly bound
// operations. Export has no network access and does not require credentials.
package contract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mujhtech/trest/httpadapter"
	"github.com/mujhtech/trest/internal/openapischema"
	"github.com/mujhtech/trest/operation"
)

// Info identifies an application contract.
type Info struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

// Export returns an OpenAPI 3.1.1 document for a frozen registry and its explicit
// HTTP bindings. Output is stable across registration and binding order. Access
// requirements are documented in x-trest-access; authentication transport remains
// application-owned, so Export does not invent an OpenAPI security scheme.
func Export(registry *operation.Registry, bindings []httpadapter.Binding, info Info) ([]byte, error) {
	if registry == nil || !registry.IsFrozen() {
		return nil, fmt.Errorf("contract: registry must be frozen")
	}
	if strings.TrimSpace(info.Title) == "" || strings.TrimSpace(info.Version) == "" {
		return nil, fmt.Errorf("contract: title and version are required")
	}
	bindings = append([]httpadapter.Binding(nil), bindings...)
	sort.Slice(bindings, func(i, j int) bool {
		if bindings[i].Path == bindings[j].Path {
			return bindings[i].Method < bindings[j].Method
		}
		return bindings[i].Path < bindings[j].Path
	})
	paths := make(map[string]any)
	schemas := map[string]any{"TrestError": errorSchema()}
	seenOperations := make(map[string]bool)
	for _, binding := range bindings {
		if err := validateBinding(binding); err != nil {
			return nil, err
		}
		metadata, ok := registry.Lookup(binding.OperationID)
		if !ok {
			return nil, fmt.Errorf("contract: unknown operation %q", binding.OperationID)
		}
		if seenOperations[binding.OperationID] {
			return nil, fmt.Errorf("contract: operation %q has more than one http binding", binding.OperationID)
		}
		seenOperations[binding.OperationID] = true
		inputName, outputName := metadata.ID+".Input", metadata.ID+".Output"
		if !json.Valid(metadata.InputSchema) || !json.Valid(metadata.OutputSchema) {
			return nil, fmt.Errorf("contract: operation %q has invalid schema metadata", metadata.ID)
		}
		for _, entry := range []struct {
			name string
			raw  json.RawMessage
		}{{inputName, metadata.InputSchema}, {outputName, metadata.OutputSchema}} {
			placed, err := openapischema.Place(entry.raw, entry.name, schemas)
			if err != nil {
				return nil, err
			}
			if _, exists := schemas[entry.name]; !exists {
				schemas[entry.name] = placed
			}
		}
		pathItem, ok := paths[binding.Path].(map[string]any)
		if !ok {
			pathItem = make(map[string]any)
			paths[binding.Path] = pathItem
		}
		method := strings.ToLower(binding.Method)
		if _, exists := pathItem[method]; exists {
			return nil, fmt.Errorf("contract: duplicate route %s %s", binding.Method, binding.Path)
		}
		responses := errorResponses(binding.Method)
		responses[strconv.Itoa(binding.SuccessStatus)] = map[string]any{
			"description": "Successful operation",
			"headers":     requestIDHeader(),
			"content":     map[string]any{"application/json": map[string]any{"schema": reference(outputName)}},
		}
		item := map[string]any{
			"operationId":        metadata.ID,
			"summary":            metadata.Summary,
			"responses":          responses,
			"x-trest-access":     metadata.Access,
			"x-trest-timeout-ms": metadata.Timeout.Milliseconds(),
		}
		if metadata.SideEffect != "" {
			item["x-trest-side-effect"] = metadata.SideEffect
		}
		if binding.Method == "POST" || binding.Method == "PUT" || binding.Method == "PATCH" {
			item["requestBody"] = map[string]any{
				"required": true,
				"content":  map[string]any{"application/json": map[string]any{"schema": reference(inputName)}},
			}
		} else {
			if err := requireEmptyInput(metadata.InputSchema); err != nil {
				return nil, fmt.Errorf("contract: operation %q: %w", metadata.ID, err)
			}
		}
		pathItem[method] = item
	}
	document := map[string]any{
		"openapi":           "3.1.1",
		"jsonSchemaDialect": "https://json-schema.org/draft/2020-12/schema",
		"info":              info,
		"paths":             paths,
		"components":        map[string]any{"schemas": schemas},
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("contract: encode openapi: %w", err)
	}
	return append(data, '\n'), nil
}

func validateBinding(binding httpadapter.Binding) error {
	switch binding.Method {
	case "GET", "DELETE", "POST", "PUT", "PATCH":
	default:
		return fmt.Errorf("contract: unsupported method %q", binding.Method)
	}
	if binding.Path == "" || !strings.HasPrefix(binding.Path, "/") {
		return fmt.Errorf("contract: invalid literal route %q", binding.Path)
	}
	if binding.Path != "/" {
		for _, segment := range strings.Split(binding.Path[1:], "/") {
			if segment == "" || segment == "." || segment == ".." {
				return fmt.Errorf("contract: invalid literal route %q", binding.Path)
			}
			for _, c := range segment {
				if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("-._~", c) {
					return fmt.Errorf("contract: invalid literal route %q", binding.Path)
				}
			}
		}
	}
	switch binding.SuccessStatus {
	case 200, 201, 202:
	default:
		return fmt.Errorf("contract: unsupported success status %d", binding.SuccessStatus)
	}
	return nil
}

func requireEmptyInput(data json.RawMessage) error {
	var shape struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(data, &shape); err != nil || shape.Type != "object" || len(shape.Properties) != 0 {
		return fmt.Errorf("bodyless routes require empty object input")
	}
	return nil
}

func reference(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func requestIDHeader() map[string]any {
	return map[string]any{"X-Request-ID": map[string]any{
		"description": "Server-generated request correlation ID",
		"schema":      map[string]any{"type": "string"},
	}}
}

func errorResponses(method string) map[string]any {
	statuses := map[string]string{
		"400": "Invalid input", "401": "Unauthenticated", "403": "Forbidden",
		"404": "Not found", "408": "Request canceled", "409": "Conflict",
		"413": "Request body too large", "429": "Invocation limit reached",
		"500": "Internal error", "503": "Unavailable", "504": "Invocation deadline exceeded",
	}
	if method == "POST" || method == "PUT" || method == "PATCH" {
		statuses["415"] = "Unsupported media type"
	}
	responses := make(map[string]any, len(statuses))
	for status, description := range statuses {
		responses[status] = map[string]any{
			"description": description,
			"headers":     requestIDHeader(),
			"content":     map[string]any{"application/json": map[string]any{"schema": reference("TrestError")}},
		}
	}
	return responses
}

func errorSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"code", "message", "request_id"},
		"properties": map[string]any{
			"code": map[string]any{"type": "string", "enum": []string{
				"invalid_input", "unauthenticated", "forbidden", "not_found", "conflict", "rate_limited", "unavailable", "internal", "deadline_exceeded", "canceled",
			}},
			"message":    map[string]any{"type": "string"},
			"request_id": map[string]any{"type": "string"},
			"fields": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object", "additionalProperties": false,
					"required": []string{"path", "code", "message"},
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"code":    map[string]any{"type": "string"},
						"message": map[string]any{"type": "string"},
					},
				},
			},
		},
	}
}
