package trest

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"trest.local/trest/internal/docsui"
	"trest.local/trest/internal/openapischema"
)

// OpenAPI returns a deterministic OpenAPI 3.1 contract from the compiled routes.
// Declarative constraints are the same ones used for request/response validation.
// Arbitrary business validators are not reverse-engineered into JSON Schema.
func (a *App) OpenAPI() ([]byte, error) {
	paths := map[string]any{}
	schemas := map[string]any{}
	for _, route := range a.routes {
		contractPath := pathParameter.ReplaceAllString(route.Path, "{$1}")
		item, ok := paths[contractPath].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[contractPath] = item
		}
		responses := map[string]any{}
		for _, status := range []int{400, 401, 403, 404, 408, 409, 413, 415, 429, 500, 503, 504} {
			responses[strconv.Itoa(status)] = map[string]any{"description": http.StatusText(status), "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/TrestError"}}}}
		}
		declared := route.Responses
		if len(declared) == 0 {
			declared = []ResponseInfo{{Status: route.Status, ContentType: route.ContentType, Schema: route.OutputSchema}}
		}
		for _, response := range declared {
			success := map[string]any{"description": http.StatusText(response.Status)}
			if response.ContentType != "" {
				shape := response.Schema
				if len(shape) == 0 {
					shape = json.RawMessage(`{"type":"string","format":"binary"}`)
				}
				name := "trest.route." + route.Name + ".output"
				if len(route.Responses) > 0 {
					name += "." + strconv.Itoa(response.Status)
				}
				placed, err := openapischema.Place(shape, name, schemas)
				if err != nil {
					return nil, err
				}
				success["content"] = map[string]any{response.ContentType: map[string]any{"schema": placed}}
			}
			responses[strconv.Itoa(response.Status)] = success
		}

		operation := map[string]any{"operationId": route.Name, "summary": route.Summary, "responses": responses, "x-trest-native": route.NativeOnly}
		if route.Protocol != "" {
			operation["x-trest-protocol"] = route.Protocol
		}
		if route.Stream != nil {
			placed, err := openapischema.Place(route.Stream.Schema, "trest.route."+route.Name+".event", schemas)
			if err != nil {
				return nil, err
			}
			operation["x-trest-stream"] = map[string]any{"format": route.Stream.Format, "schema": placed}
		}
		if route.WebSocket != nil {
			in, err := openapischema.Place(route.WebSocket.Input, "trest.route."+route.Name+".websocket.input", schemas)
			if err != nil {
				return nil, err
			}
			out, err := openapischema.Place(route.WebSocket.Output, "trest.route."+route.Name+".websocket.output", schemas)
			if err != nil {
				return nil, err
			}
			operation["x-trest-websocket"] = map[string]any{"input": in, "output": out}
		}
		if route.OperationID != "" {
			operation["x-trest-operation"] = route.OperationID
		}
		if route.Authenticated {
			operation["x-trest-authenticated"] = true
		}
		if route.Permission != "" {
			operation["x-trest-permission"] = route.Permission
		}
		if len(route.Parameters) > 0 {
			parameters := make([]map[string]any, 0, len(route.Parameters))
			for i, p := range route.Parameters {
				placed, err := openapischema.Place(p.Schema, "trest.route."+route.Name+".parameter."+strconv.Itoa(i), schemas)
				if err != nil {
					return nil, err
				}
				parameters = append(parameters, map[string]any{"name": p.Name, "in": p.In, "required": p.Required, "schema": placed})
			}
			operation["parameters"] = parameters
		}
		if len(route.BodySchema) > 0 {
			placed, err := openapischema.Place(route.BodySchema, "trest.route."+route.Name+".input", schemas)
			if err != nil {
				return nil, err
			}
			operation["requestBody"] = map[string]any{"required": route.BodyRequired, "content": map[string]any{route.BodyContentType: map[string]any{"schema": placed}}}
		}
		item[strings.ToLower(route.Method)] = operation
	}
	fields := map[string]any{"type": "array", "items": map[string]any{"type": "object", "required": []string{"path", "code", "message"}, "properties": map[string]any{"path": map[string]string{"type": "string"}, "code": map[string]string{"type": "string"}, "message": map[string]string{"type": "string"}}}}
	errorSchema := map[string]any{"type": "object", "required": []string{"code", "message", "request_id"}, "properties": map[string]any{"code": map[string]any{"type": "string", "enum": []string{"invalid_input", "unauthenticated", "forbidden", "not_found", "conflict", "rate_limited", "unavailable", "internal", "deadline_exceeded", "canceled"}}, "message": map[string]string{"type": "string"}, "request_id": map[string]string{"type": "string"}, "fields": fields}}
	schemas["TrestError"] = errorSchema
	document := map[string]any{"openapi": "3.1.1", "jsonSchemaDialect": "https://json-schema.org/draft/2020-12/schema", "info": map[string]string{"title": a.title, "version": a.version}, "paths": paths, "components": map[string]any{"schemas": schemas}}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

type documentation struct{ path string }

// Docs mounts a self-contained interactive explorer at path and its contract at
// path/openapi.json. No CDN or external service is used. Put it inside an
// authenticated group when API metadata should not be publicly accessible.
func Docs(path string) Component { return &documentation{path: path} }
func (d *documentation) mount(b *builder, s scope) error {
	if d.path == "" || d.path == "/" {
		return errors.New("trest: docs requires a non-root path")
	}
	base, err := joinRoute(s.prefix, strings.TrimSuffix(d.path, "/"))
	if err != nil {
		return err
	}
	spec := base + "/openapi.json"
	page := Raw("GET", strings.TrimSuffix(d.path, "/"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(docsui.Page(b.app.title, spec))
	}))
	contract := Raw("GET", strings.TrimSuffix(d.path, "/")+"/openapi.json", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := b.app.OpenAPI()
		if err != nil {
			writeError(w, r, s, err, 0)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	page.hidden = true
	contract.hidden = true
	if err := page.mount(b, s); err != nil {
		return err
	}
	return contract.mount(b, s)
}
