// Package webinput compiles typed HTTP input declarations for the public app API.
package webinput

import (
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"trest.local/trest/internal/schema"
)

// Parameter describes one path, query, header, or cookie binding.
type Parameter struct {
	Name     string          `json:"name"`
	In       string          `json:"in"`
	Required bool            `json:"required"`
	Schema   json.RawMessage `json:"schema"`
}

// Plan owns immutable request binding instructions compiled before serving.
// Metadata slices are detached from the private instructions used by Decode.
type Plan struct {
	Parameters      []Parameter
	BodySchema      json.RawMessage
	BodyContentType string
	BodyRequired    bool
	NativeOnly      bool
	typ             reflect.Type
	fields          []field
	wholeBody       bool
	bodyIndex       int
	bodyValidator   *schema.Validator
	bodyType        reflect.Type
	form            bool
	multipart       bool
	queries         map[string]bool
	formNames       map[string]bool
}

type field struct {
	index       int
	name        string
	source      string
	typ         reflect.Type
	required    bool
	defaultText *string
	validator   *schema.Validator
	wireKind    string
	itemKind    string
	file        bool
	manyFiles   bool
	minItems    int
	maxItems    int
}

var fileType = reflect.TypeFor[*multipart.FileHeader]()
var fileSliceType = reflect.TypeFor[[]*multipart.FileHeader]()
var timeType = reflect.TypeFor[time.Time]()

// Compile verifies a struct's source tags and matches path fields to route captures.
// Without source tags, POST/PUT/PATCH use the entire struct as a JSON body.
func Compile(typ reflect.Type, method, path string) (*Plan, error) {
	if typ == nil || typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("request input must be a struct")
	}
	p := &Plan{typ: typ, bodyIndex: -1, queries: make(map[string]bool), formNames: make(map[string]bool)}
	captures, err := pathCaptures(path)
	if err != nil {
		return nil, err
	}
	explicit := false
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		for _, source := range []string{"path", "query", "header", "cookie", "body", "form"} {
			if _, ok := f.Tag.Lookup(source); ok {
				explicit = true
			}
		}
	}
	if !explicit {
		if len(captures) > 0 {
			return nil, fmt.Errorf("path captures require matching path fields")
		}
		if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
			p.wholeBody = true
			p.BodyRequired = true
			p.BodyContentType = "application/json"
			p.bodyType = typ
			p.BodySchema, err = schema.Generate(typ)
			if err != nil {
				return nil, err
			}
			p.bodyValidator, err = schema.Compile(p.BodySchema)
			return p, err
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.PkgPath == "" && f.Tag.Get("json") != "-" {
				return nil, fmt.Errorf("field %s requires a path, query, header, cookie, body, or form tag", f.Name)
			}
		}
		return p, nil
	}
	seen := make(map[string]bool)
	bodyProperties := make(map[string]json.RawMessage)
	var bodyRequired []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue
		}
		if f.Anonymous {
			return nil, fmt.Errorf("embedded request fields are unsupported: %s", f.Name)
		}
		var source, name string
		for _, candidate := range []string{"path", "query", "header", "cookie", "body", "form"} {
			if value, ok := f.Tag.Lookup(candidate); ok {
				if source != "" {
					return nil, fmt.Errorf("field %s has multiple input sources", f.Name)
				}
				source, name = candidate, value
			}
		}
		if source == "" {
			if f.Tag.Get("json") == "-" {
				continue
			}
			return nil, fmt.Errorf("field %s has no input source", f.Name)
		}
		if name == "" || strings.ContainsAny(name, ",\r\n\t ") {
			return nil, fmt.Errorf("field %s has an invalid %s name", f.Name, source)
		}
		if source == "body" {
			if f.Tag.Get("json") == "-" {
				return nil, fmt.Errorf("body field %s must remain in its JSON contract", f.Name)
			}
			if name != "json" {
				return nil, fmt.Errorf("body tag must be json")
			}
			if p.bodyIndex >= 0 || p.form {
				return nil, fmt.Errorf("request must have one body representation")
			}
			p.bodyIndex = i
			p.bodyType = f.Type
			p.BodyRequired = true
			p.BodyContentType = "application/json"
			p.BodySchema, err = schema.GenerateField(f)
			if err != nil {
				return nil, err
			}
			p.bodyValidator, err = schema.Compile(p.BodySchema)
			if err != nil {
				return nil, err
			}
			continue
		}
		if source == "header" {
			name = http.CanonicalHeaderKey(name)
			if !validHeaderName(name) {
				return nil, fmt.Errorf("invalid header name on %s", f.Name)
			}
		}
		key := source + ":" + name
		if seen[key] {
			return nil, fmt.Errorf("duplicate %s binding %s", source, name)
		}
		seen[key] = true
		binding := field{index: i, name: name, source: source, typ: f.Type, required: f.Type.Kind() != reflect.Pointer, maxItems: -1}
		if tag, ok := f.Tag.Lookup("required"); ok {
			if tag != "true" && tag != "false" {
				return nil, fmt.Errorf("required on %s must be true or false", f.Name)
			}
			binding.required = tag == "true"
		}
		if text, ok := f.Tag.Lookup("default"); ok {
			binding.defaultText = &text
			binding.required = false
		}
		if source == "path" {
			if !captures[name] {
				return nil, fmt.Errorf("path field %s has no route capture", name)
			}
			delete(captures, name)
			binding.required = true
			if binding.defaultText != nil {
				return nil, fmt.Errorf("path fields cannot have defaults")
			}
		}
		if source == "form" {
			if p.bodyIndex >= 0 {
				return nil, fmt.Errorf("form fields cannot be mixed with a JSON body")
			}
			p.form = true
			p.BodyRequired = true
			p.formNames[name] = true
		}
		binding.file = f.Type == fileType
		binding.manyFiles = f.Type == fileSliceType
		var spec json.RawMessage
		if binding.file || binding.manyFiles {
			if source != "form" || f.Tag.Get("json") != "-" {
				return nil, fmt.Errorf("upload field %s requires form and json:\"-\" tags", f.Name)
			}
			if binding.defaultText != nil {
				return nil, fmt.Errorf("uploads cannot have defaults")
			}
			p.NativeOnly = true
			p.multipart = true
			if binding.manyFiles {
				var min, max *int
				for _, item := range []struct {
					name string
					dst  **int
				}{{"minItems", &min}, {"maxItems", &max}} {
					if value, ok := f.Tag.Lookup(item.name); ok {
						n, e := strconv.Atoi(value)
						if e != nil || n < 0 {
							return nil, fmt.Errorf("invalid upload count constraint")
						}
						*item.dst = &n
					}
				}
				if min != nil {
					binding.minItems = *min
				}
				if max != nil {
					binding.maxItems = *max
				}
				if max != nil && binding.minItems > *max {
					return nil, fmt.Errorf("upload minItems exceeds maxItems")
				}
				shape := map[string]any{"type": "array", "items": map[string]string{"type": "string", "format": "binary"}}
				if min != nil {
					shape["minItems"] = *min
				}
				if max != nil {
					shape["maxItems"] = *max
				}
				spec, err = json.Marshal(shape)
			} else {
				spec = json.RawMessage(`{"type":"string","format":"binary"}`)
			}
		} else {
			if f.Tag.Get("json") == "-" {
				return nil, fmt.Errorf("bound field %s must remain in its JSON contract", f.Name)
			}
			if !parameterType(f.Type, source == "query" || source == "form" || source == "header") {
				return nil, fmt.Errorf("unsupported %s type on %s", source, f.Name)
			}
			spec, err = schema.GenerateField(f)
			if err != nil {
				return nil, err
			}
			binding.validator, err = schema.Compile(spec)
			if err == nil {
				var supported bool
				binding.wireKind, binding.itemKind, supported = binding.validator.ParameterShape()
				if !supported || schema.HasCustomCodec(f.Type) && binding.wireKind == "array" {
					return nil, fmt.Errorf("%s field %s requires a scalar wire schema or a collection of scalar values", source, f.Name)
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if source == "query" {
			p.queries[name] = true
		}
		if source == "form" {
			bodyProperties[name] = spec
			if binding.required {
				bodyRequired = append(bodyRequired, name)
			}
		} else {
			p.Parameters = append(p.Parameters, Parameter{Name: name, In: source, Required: binding.required, Schema: spec})
		}
		p.fields = append(p.fields, binding)
	}
	if len(captures) > 0 {
		var names []string
		for name := range captures {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("route captures have no input fields: %s", strings.Join(names, ", "))
	}
	if p.form {
		p.BodyContentType = "application/x-www-form-urlencoded"
		if p.multipart {
			p.BodyContentType = "multipart/form-data"
		}
		sort.Strings(bodyRequired)
		shape := map[string]any{"type": "object", "properties": bodyProperties, "additionalProperties": false}
		if len(bodyRequired) > 0 {
			shape["required"] = bodyRequired
		}
		p.BodySchema, err = json.Marshal(shape)
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(p.Parameters, func(i, j int) bool {
		if p.Parameters[i].In == p.Parameters[j].In {
			return p.Parameters[i].Name < p.Parameters[j].Name
		}
		return p.Parameters[i].In < p.Parameters[j].In
	})
	return p, nil
}

func parameterType(t reflect.Type, allowSlice bool) bool {
	if t.Kind() == reflect.Pointer {
		return parameterType(t.Elem(), allowSlice)
	}
	if t == timeType {
		return true
	}
	if schema.HasCustomCodec(t) {
		return true
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		return allowSlice && parameterType(t.Elem(), false)
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
func pathCaptures(path string) (map[string]bool, error) {
	captures := make(map[string]bool)
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("route path must start with /")
	}
	for _, segment := range strings.Split(path, "/") {
		if !strings.ContainsAny(segment, "{}") {
			continue
		}
		if segment == "{$}" {
			continue
		}
		if !strings.HasPrefix(segment, "{") || !strings.HasSuffix(segment, "}") {
			return nil, fmt.Errorf("invalid route capture")
		}
		name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}"), "...")
		if name == "" || strings.ContainsAny(name, "{}./ ") {
			return nil, fmt.Errorf("invalid route capture")
		}
		if captures[name] {
			return nil, fmt.Errorf("duplicate route capture %s", name)
		}
		captures[name] = true
	}
	return captures, nil
}
func validHeaderName(name string) bool {
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return name != ""
}
