// Package clientgen creates standalone typed clients from Trest OpenAPI contracts.
// Generation is deterministic, requires no network, and rejects unsupported
// schemas instead of silently replacing known types with an untyped payload.
package clientgen

import (
	"encoding/json"
	"fmt"
	"go/token"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type shape struct {
	Ref        string            `json:"$ref"`
	Type       any               `json:"type"`
	Properties map[string]*shape `json:"properties"`
	Required   []string          `json:"required"`
	Items      *shape            `json:"items"`
	Additional json.RawMessage   `json:"additionalProperties"`
	Enum       []any             `json:"enum"`
	Format     string            `json:"format"`
	Minimum    json.Number       `json:"minimum"`
	Maximum    json.Number       `json:"maximum"`
	AnyOf      []*shape          `json:"anyOf"`
	OneOf      json.RawMessage   `json:"oneOf"`
	AllOf      json.RawMessage   `json:"allOf"`
	Not        json.RawMessage   `json:"not"`
}

type parameter struct {
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
	Schema   *shape `json:"schema"`
	Ref      string `json:"$ref"`
	Style    string `json:"style"`
	Explode  *bool  `json:"explode"`
}

type media struct {
	Schema *shape `json:"schema"`
}
type body struct {
	Required bool             `json:"required"`
	Content  map[string]media `json:"content"`
	Ref      string           `json:"$ref"`
}
type response struct {
	Content map[string]media `json:"content"`
	Ref     string           `json:"$ref"`
}
type streamInfo struct {
	Format string `json:"format"`
	Schema *shape `json:"schema"`
}
type socketInfo struct {
	Input  *shape `json:"input"`
	Output *shape `json:"output"`
}
type operation struct {
	Stream      *streamInfo         `json:"x-trest-stream"`
	Socket      *socketInfo         `json:"x-trest-websocket"`
	ID          string              `json:"operationId"`
	Parameters  []parameter         `json:"parameters"`
	Body        *body               `json:"requestBody"`
	Responses   map[string]response `json:"responses"`
	Protocol    string              `json:"x-trest-protocol"`
	Method      string
	Path        string
	Export      string
	BodyKind    string
	BodyType    *shape
	FormFields  []formField
	TypedForm   bool
	OutputKind  string
	OutputType  *shape
	Statuses    []int
	Outputs     []outputVariant
	MixedOutput bool
}

type outputVariant struct {
	Status int
	Kind   string
	Schema *shape
}

type formField struct {
	Name      string
	Schema    *shape
	Required  bool
	File      bool
	ManyFiles bool
	Nullable  bool
}
type document struct {
	OpenAPI    string                                `json:"openapi"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Components struct {
		Schemas map[string]*shape `json:"schemas"`
	} `json:"components"`
	operations          []operation
	websocketOperations []operation
}

var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
var routeParameter = regexp.MustCompile(`\{([^{}]+)\}`)

func parse(data []byte) (*document, error) {
	if len(data) > 16<<20 {
		return nil, fmt.Errorf("clientgen: contract exceeds 16 MiB")
	}
	var d document
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("clientgen: invalid contract: %w", err)
	}
	if !strings.HasPrefix(d.OpenAPI, "3.1.") {
		return nil, fmt.Errorf("clientgen: OpenAPI 3.1 is required")
	}
	if d.Paths == nil {
		return nil, fmt.Errorf("clientgen: paths are required")
	}
	componentNames := map[string]string{}
	for _, name := range keys(d.Components.Schemas) {
		n := goName(name)
		if n == "" {
			return nil, fmt.Errorf("clientgen: invalid schema name %q", name)
		}
		if previous, ok := componentNames[n]; ok {
			return nil, fmt.Errorf("clientgen: schema names %q and %q collide", previous, name)
		}
		componentNames[n] = name
		if err := d.checkShape(d.Components.Schemas[name], make(map[*shape]int), 0, 0); err != nil {
			return nil, fmt.Errorf("clientgen: schema %s: %w", name, err)
		}
	}
	seen := map[string]bool{}
	for _, path := range keys(d.Paths) {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\r\n") {
			return nil, fmt.Errorf("clientgen: invalid route %q", path)
		}
		item := d.Paths[path]
		var common []parameter
		if raw, ok := item["parameters"]; ok {
			if err := json.Unmarshal(raw, &common); err != nil {
				return nil, fmt.Errorf("clientgen: path parameters: %w", err)
			}
		}
		for _, method := range []string{"get", "post", "put", "patch", "delete", "head", "options"} {
			raw, ok := item[method]
			if !ok {
				continue
			}
			var op operation
			if err := json.Unmarshal(raw, &op); err != nil {
				return nil, fmt.Errorf("clientgen: operation: %w", err)
			}
			if !identifier.MatchString(op.ID) || reservedOperation(op.ID) {
				return nil, fmt.Errorf("clientgen: operationId %q must be a unique client identifier", op.ID)
			}
			op.Export = goName(op.ID)
			if seen[op.Export] || op.Export == "HTTP" || op.Export == "Headers" || op.Export == "MaxResponseBytes" || op.Export == "MaxRequestBytes" || op.Export == "MaxEventBytes" || op.Export == "MaxMessageBytes" || op.Export == "SocketDialer" {
				return nil, fmt.Errorf("clientgen: duplicate or colliding operationId %q", op.ID)
			}
			seen[op.Export] = true
			op.Method, op.Path = strings.ToUpper(method), path
			if op.Protocol == "websocket" && op.Socket == nil {
				d.websocketOperations = append(d.websocketOperations, op)
				continue
			}
			op.Parameters = mergeParameters(common, op.Parameters)
			if err := d.prepare(&op); err != nil {
				return nil, fmt.Errorf("clientgen: %s: %w", op.ID, err)
			}
			d.operations = append(d.operations, op)
		}
	}
	sort.Slice(d.operations, func(i, j int) bool { return d.operations[i].ID < d.operations[j].ID })
	sort.Slice(d.websocketOperations, func(i, j int) bool {
		return d.websocketOperations[i].ID < d.websocketOperations[j].ID
	})
	return &d, nil
}

func mergeParameters(common, local []parameter) []parameter {
	merged := make(map[string]parameter)
	for _, p := range append(append([]parameter{}, common...), local...) {
		merged[p.In+"\x00"+p.Name] = p
	}
	out := make([]parameter, 0, len(merged))
	for _, k := range keys(merged) {
		out = append(out, merged[k])
	}
	return out
}

func (d *document) prepare(op *operation) error {
	if op.Stream != nil {
		if op.Stream.Format != "sse" || op.Socket != nil {
			return fmt.Errorf("unsupported stream metadata")
		}
		if err := d.checkShape(op.Stream.Schema, make(map[*shape]int), 0, 0); err != nil {
			return err
		}
	}
	if op.Socket != nil {
		if op.Protocol != "websocket" || op.Method != "GET" || op.Body != nil {
			return fmt.Errorf("typed WebSocket requires a GET upgrade without a request body")
		}
		for _, s := range []*shape{op.Socket.Input, op.Socket.Output} {
			if err := d.checkShape(s, make(map[*shape]int), 0, 0); err != nil {
				return err
			}
		}
	}
	paths := map[string]bool{}
	for _, p := range op.Parameters {
		if p.Ref != "" || p.Schema == nil {
			return fmt.Errorf("parameter %q needs an inline schema", p.Name)
		}
		if p.Name == "" || strings.ContainsAny(p.Name, "\r\n") {
			return fmt.Errorf("invalid parameter name")
		}
		switch p.In {
		case "path", "query", "header", "cookie":
		default:
			return fmt.Errorf("unsupported parameter location %q", p.In)
		}
		if err := d.checkShape(p.Schema, make(map[*shape]int), 0, 0); err != nil {
			return err
		}
		base, err := d.resolve(p.Schema)
		if err != nil {
			return err
		}
		typ, nullable := base.kind()
		if p.In == "path" && nullable {
			return fmt.Errorf("path parameters cannot be nullable")
		}
		if typ == "array" {
			if p.In != "query" && p.In != "header" {
				return fmt.Errorf("array parameters are supported only in query and headers")
			}
			item, err := d.resolve(base.Items)
			if err != nil {
				return err
			}
			child, _ := item.kind()
			if child != "string" && child != "integer" && child != "number" && child != "boolean" {
				return fmt.Errorf("parameter arrays require scalar items")
			}
		} else if typ != "string" && typ != "integer" && typ != "number" && typ != "boolean" {
			return fmt.Errorf("parameter %q requires a scalar or query/header array", p.Name)
		}
		if p.Style != "" && ((p.In == "query" && p.Style != "form") || (p.In != "query" && p.Style != "simple" && p.In != "cookie") || (p.In == "cookie" && p.Style != "form")) {
			return fmt.Errorf("unsupported parameter style")
		}
		if p.In == "query" && p.Explode != nil && !*p.Explode {
			return fmt.Errorf("query parameters require explode=true")
		}
		if p.In == "path" {
			if !p.Required {
				return fmt.Errorf("path parameter %q must be required", p.Name)
			}
			paths[p.Name] = true
		}
	}
	for _, match := range routeParameter.FindAllStringSubmatch(op.Path, -1) {
		if !paths[match[1]] {
			return fmt.Errorf("path parameter %q has no declaration", match[1])
		}
		delete(paths, match[1])
	}
	if len(paths) > 0 {
		return fmt.Errorf("declared path parameter is absent from route")
	}
	if strings.ContainsAny(routeParameter.ReplaceAllString(op.Path, "x"), "{}") {
		return fmt.Errorf("malformed route template")
	}
	if op.Body != nil {
		if op.Body.Ref != "" || len(op.Body.Content) != 1 {
			return fmt.Errorf("request body needs exactly one inline media type")
		}
		for kind, media := range op.Body.Content {
			op.BodyKind, op.BodyType = kind, media.Schema
			switch kind {
			case "application/json":
				if media.Schema == nil {
					return fmt.Errorf("JSON body schema is missing")
				}
				if err := d.checkShape(media.Schema, make(map[*shape]int), 0, 0); err != nil {
					return err
				}
			case "application/x-www-form-urlencoded", "multipart/form-data":
				if err := d.prepareForm(op); err != nil {
					return err
				}
			case "application/octet-stream", "text/plain":
			default:
				return fmt.Errorf("unsupported request media type %q", kind)
			}
		}
	}
	if op.Socket != nil {
		return nil
	}
	for _, status := range keys(op.Responses) {
		if len(status) != 3 {
			continue
		}
		declaredSuccess := status[0] == '2' || status[0] == '3'
		numericSuffix := status[1] >= '0' && status[1] <= '9' && status[2] >= '0' && status[2] <= '9'
		if !declaredSuccess || !numericSuffix {
			continue
		}
		code := int(status[0]-'0')*100 + int(status[1]-'0')*10 + int(status[2]-'0')
		resp := op.Responses[status]
		if resp.Ref != "" || len(resp.Content) > 1 {
			return fmt.Errorf("success response needs at most one inline media type")
		}
		kind := "empty"
		var schema *shape
		for mime, m := range resp.Content {
			kind = mime
			schema = m.Schema
		}
		// These responses never carry a body, even when OpenAPI describes
		// a representation schema for their metadata.
		if op.Method == "HEAD" || code == 204 || code == 205 || code == 304 {
			kind, schema = "empty", nil
		}
		if op.Stream != nil && kind != "text/event-stream" {
			return fmt.Errorf("typed event streams require text/event-stream success responses")
		}
		if kind == "application/json" {
			if schema == nil {
				return fmt.Errorf("JSON response schema missing")
			}
			if err := d.checkShape(schema, make(map[*shape]int), 0, 0); err != nil {
				return err
			}
		}
		if len(op.Statuses) > 0 {
			a, _ := json.Marshal(op.OutputType)
			b, _ := json.Marshal(schema)
			if op.OutputKind != kind || string(a) != string(b) {
				op.MixedOutput = true
			}
		} else {
			op.OutputKind, op.OutputType = kind, schema
		}
		op.Statuses = append(op.Statuses, code)
		op.Outputs = append(op.Outputs, outputVariant{Status: code, Kind: kind, Schema: schema})
	}
	if len(op.Statuses) == 0 {
		return fmt.Errorf("a concrete 2xx or 3xx response is required")
	}
	return nil
}

func (d *document) prepareForm(op *operation) error {
	if op.BodyType == nil {
		return nil
	}
	object, err := d.resolve(op.BodyType)
	if err != nil {
		return err
	}
	kind, nullable := object.kind()
	if kind != "object" || nullable || len(object.Additional) > 0 && string(object.Additional) != "false" {
		return nil
	}
	fields := make([]formField, 0, len(object.Properties))
	for _, name := range keys(object.Properties) {
		property := object.Properties[name]
		base, err := d.resolve(property)
		if err != nil {
			return err
		}
		kind, nullable := base.kind()
		field := formField{Name: name, Schema: property, Required: required(object, name), Nullable: nullable}
		if kind == "array" {
			base, err = d.resolve(base.Items)
			if err != nil {
				return err
			}
			kind, _ = base.kind()
			field.ManyFiles = kind == "string" && base.Format == "binary"
		}
		field.File = kind == "string" && base.Format == "binary"
		if field.File && op.BodyKind != "multipart/form-data" {
			return nil
		}
		switch kind {
		case "string", "integer", "number", "boolean":
		default:
			return nil
		}
		fields = append(fields, field)
	}
	if err := d.checkShape(op.BodyType, make(map[*shape]int), 0, 0); err != nil {
		return err
	}
	op.FormFields, op.TypedForm = fields, true
	return nil
}

func (s *shape) kind() (string, bool) {
	if s == nil {
		return "", false
	}
	switch value := s.Type.(type) {
	case string:
		return value, value == "null"
	case []any:
		typ := ""
		nullable := false
		for _, v := range value {
			if v == "null" {
				nullable = true
			} else if x, ok := v.(string); ok {
				if typ != "" {
					return "unsupported", false
				}
				typ = x
			} else {
				return "unsupported", false
			}
		}
		return typ, nullable
	}
	return "", false
}
func (d *document) resolve(s *shape) (*shape, error) {
	seen := map[string]bool{}
	nullable := false
	for s != nil {
		ref, nullableRef := s.reference()
		if ref == "" {
			break
		}
		nullable = nullable || nullableRef
		if seen[ref] {
			return nil, fmt.Errorf("cyclic scalar reference")
		}
		seen[ref] = true
		name, err := refName(ref)
		if err != nil {
			return nil, err
		}
		var ok bool
		s, ok = d.Components.Schemas[name]
		if !ok {
			return nil, fmt.Errorf("unknown schema reference %q", name)
		}
	}
	if s == nil {
		return nil, fmt.Errorf("missing schema")
	}
	if nullable {
		resolved := *s
		typ, _ := s.kind()
		resolved.Type = []any{typ, "null"}
		return &resolved, nil
	}
	return s, nil
}
func refName(ref string) (string, error) {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("external or unsupported schema reference %q", ref)
	}
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(ref, prefix), "~1", "/"), "~0", "~"), nil
}

// reference recognizes a named type and the nullable-reference form emitted for
// recursive Go pointers. Other unions still require an explicit response type.
func (s *shape) reference() (string, bool) {
	if s == nil {
		return "", false
	}
	if s.Ref != "" {
		return s.Ref, false
	}
	if len(s.AnyOf) != 2 {
		return "", false
	}
	for i, variant := range s.AnyOf {
		other := s.AnyOf[1-i]
		if variant == nil || other == nil || variant.Ref == "" {
			continue
		}
		if len(variant.AnyOf)+len(variant.OneOf)+len(variant.AllOf)+len(variant.Not) > 0 {
			continue
		}
		if len(other.AnyOf)+len(other.OneOf)+len(other.AllOf)+len(other.Not) > 0 {
			continue
		}
		if typ, _ := other.kind(); typ == "null" && other.Ref == "" {
			return variant.Ref, true
		}
	}
	return "", false
}

func (d *document) checkShape(s *shape, active map[*shape]int, depth, indirections int) error {
	if s == nil {
		return fmt.Errorf("missing schema")
	}
	if depth > 64 {
		return fmt.Errorf("schema nesting exceeds 64")
	}
	if entered, ok := active[s]; ok {
		if indirections > entered {
			return nil
		}
		return fmt.Errorf("recursive schemas require a pointer, array, or map")
	}
	active[s] = indirections
	defer delete(active, s)
	ref, nullableRef := s.reference()
	unsupportedUnion := len(s.AnyOf) > 0 && ref == ""
	if unsupportedUnion || len(s.OneOf)+len(s.AllOf)+len(s.Not) > 0 {
		return fmt.Errorf("schema combinators are unsupported")
	}
	if ref != "" {
		name, err := refName(ref)
		if err != nil {
			return err
		}
		target, ok := d.Components.Schemas[name]
		if !ok {
			return fmt.Errorf("unknown schema reference %q", name)
		}
		if nullableRef {
			indirections++
		}
		return d.checkShape(target, active, depth+1, indirections)
	}
	typ, nullable := s.kind()
	if nullable {
		indirections++
	}
	switch typ {
	case "", "null", "string", "integer", "number", "boolean":
	case "array":
		return d.checkShape(s.Items, active, depth+1, indirections+1)
	case "object":
		fieldNames := map[string]bool{}
		for _, name := range keys(s.Properties) {
			field := goName(name)
			if field == "" || fieldNames[field] || strings.ContainsAny(name, "\"\\\r\n\t") {
				return fmt.Errorf("object property %q has an invalid or colliding Go name", name)
			}
			fieldNames[field] = true
			fieldIndirections := indirections
			if !required(s, name) {
				fieldIndirections++
			}
			if err := d.checkShape(s.Properties[name], active, depth+1, fieldIndirections); err != nil {
				return err
			}
		}
		if len(s.Additional) > 0 && string(s.Additional) != "false" {
			if len(s.Properties) > 0 {
				return fmt.Errorf("objects mixing properties and additional properties are unsupported")
			}
			if string(s.Additional) != "true" {
				var child shape
				if err := json.Unmarshal(s.Additional, &child); err != nil {
					return err
				}
				return d.checkShape(&child, active, depth+1, indirections+1)
			}
		}
	default:
		return fmt.Errorf("unsupported schema type %q", typ)
	}
	return nil
}
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func goName(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !valid {
			upper = true
			continue
		}
		if b.Len() == 0 && r >= '0' && r <= '9' {
			b.WriteByte('N')
		}
		if upper {
			r = unicode.ToUpper(r)
		}
		b.WriteRune(r)
		upper = false
	}
	return b.String()
}
func required(s *shape, name string) bool {
	for _, n := range s.Required {
		if n == name {
			return true
		}
	}
	return false
}
func validPackage(name string) bool {
	return token.IsIdentifier(name) && !token.Lookup(name).IsKeyword() && name != "_" && name != "main"
}

func reservedOperation(name string) bool {
	switch name {
	case "constructor", "request", "baseURL", "config", "then":
		return true
	}
	return false
}
