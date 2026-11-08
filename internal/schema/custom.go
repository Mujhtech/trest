package schema

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"sort"
)

// Provider supplies the explicit wire contract for a custom JSON or text codec.
// TrestSchema must be pure, deterministic, and valid on a fresh zero value. The
// method may have a value or pointer receiver; no registry or import is needed.
type Provider interface {
	TrestSchema() json.RawMessage
}

// HasCustomCodec reports whether a type changes encoding/json's wire behavior.
func HasCustomCodec(t reflect.Type) bool {
	if t == nil {
		return false
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for _, codec := range customInterfaces {
		if t.Implements(codec) || reflect.PointerTo(t).Implements(codec) {
			return true
		}
	}
	return false
}

func customSchema(t reflect.Type, path string) (*node, error) {
	ptr := reflect.PointerTo(t)
	if !ptr.Implements(reflect.TypeFor[Provider]()) {
		return nil, unsupported(path, "custom json or text codec on "+t.String()+" requires TrestSchema")
	}
	jsonEncode := t.Implements(reflect.TypeFor[json.Marshaler]())
	jsonDecode := ptr.Implements(reflect.TypeFor[json.Unmarshaler]())
	textOnly := !ptr.Implements(reflect.TypeFor[json.Marshaler]()) && !jsonDecode
	textPair := t.Implements(reflect.TypeFor[encoding.TextMarshaler]()) && ptr.Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
	if (!jsonEncode || !jsonDecode) && (!textOnly || !textPair) {
		return nil, unsupported(path, "custom codecs require value encoding and pointer decoding methods for the same JSON or text format")
	}
	data, err := declaredSchema(reflect.New(t).Interface().(Provider))
	if err != nil {
		return nil, unsupported(path, err.Error())
	}
	if len(data) == 0 || len(data) > 64<<10 {
		return nil, unsupported(path, "custom schema must contain between 1 and 65536 bytes")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var duplicates []Violation
	declaration, err := readValue(dec, "", 0, &duplicates)
	if err != nil || len(duplicates) != 0 {
		return nil, unsupported(path, "custom schema must be JSON without duplicate fields or excessive nesting")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, unsupported(path, "custom schema must contain exactly one JSON value")
	}
	if err := checkDeclarationValues(declaration); err != nil {
		return nil, unsupported(path, "invalid custom schema: "+err.Error())
	}
	var declared node
	if err := decodeDeclaredSchema(data, &declared); err != nil {
		return nil, unsupported(path, "custom schema uses an unsupported declaration: "+err.Error())
	}
	v, err := Compile(data)
	if err != nil {
		return nil, unsupported(path, "invalid custom schema: "+err.Error())
	}
	if err := checkDeclaredSchema(&v.root, make(map[*node]bool)); err != nil {
		return nil, unsupported(path, "invalid custom schema: "+err.Error())
	}
	if textOnly {
		root := wireNode(&v.root)
		typ, _ := nodeType(root)
		if typ != "string" || acceptsNull(&v.root) {
			return nil, unsupported(path, "text codecs require a nonnullable string wire schema")
		}
	}
	if v.root.Ref != "" || len(v.root.AnyOf) > 0 {
		// Inline the effective root so field-level constraints can narrow it.
		// Nested recursive edges retain their explicit references.
		root := *wireNode(&v.root)
		root.Definitions = v.root.Definitions
		if v.root.Description != "" {
			root.Description = v.root.Description
		}
		if len(v.root.Default) != 0 {
			root.Default = v.root.Default
		}
		if acceptsNull(&v.root) {
			makeNullable(&root)
		}
		typ, _ := nodeType(&root)
		if typ != "object" && typ != "array" {
			root.Definitions = nil
		}
		return &root, nil
	}
	if typ, _ := nodeType(&v.root); typ != "object" && typ != "array" {
		v.root.Definitions = nil
	}
	return &v.root, nil
}

// JSON null is silently accepted for many Go fields by encoding/json, but it is
// not a valid value for schema keywords such as type, minimum, or properties.
func checkDeclarationValues(value any) error {
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("schemas must be JSON objects")
	}
	for key, value := range object {
		valid := true
		switch key {
		case "default":
			continue
		case "type":
			switch value.(type) {
			case string, []any:
			default:
				valid = false
			}
		case "$ref", "pattern", "format", "contentEncoding", "description":
			text, ok := value.(string)
			valid = ok && (key != "$ref" || text != "")
		case "minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems":
			_, valid = value.(json.Number)
		case "required":
			values, ok := value.([]any)
			valid = ok
			for _, member := range values {
				if _, ok := member.(string); !ok {
					valid = false
				}
			}
		case "enum":
			values, ok := value.([]any)
			valid = ok && len(values) > 0
			for i, member := range values {
				switch member.(type) {
				case nil, string, bool, json.Number:
				default:
					valid = false
				}
				for _, previous := range values[:i] {
					if member == nil && previous == nil || equalScalar(member, previous) {
						valid = false
					}
				}
			}
		case "properties", "$defs":
			children, ok := value.(map[string]any)
			valid = ok
			for _, child := range children {
				if err := checkDeclarationValues(child); err != nil {
					return err
				}
			}
		case "items":
			if err := checkDeclarationValues(value); err != nil {
				return err
			}
		case "additionalProperties":
			if _, ok := value.(bool); !ok {
				if err := checkDeclarationValues(value); err != nil {
					return err
				}
			}
		case "anyOf":
			children, ok := value.([]any)
			valid = ok && len(children) > 0
			for _, child := range children {
				if err := checkDeclarationValues(child); err != nil {
					return err
				}
			}
		}
		if !valid {
			return fmt.Errorf("invalid value for schema keyword %q", key)
		}
	}
	return nil
}

func declaredSchema(provider Provider) (data json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			data = nil
			err = fmt.Errorf("custom schema method panicked")
		}
	}()
	return provider.TrestSchema(), nil
}

// decodeDeclaredSchema rejects keywords that the runtime would otherwise ignore.
// additionalProperties is raw JSON in node and needs its own strict decode.
func decodeDeclaredSchema(data []byte, n *node) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(n); err != nil {
		return err
	}
	var visit func(*node) error
	visit = func(current *node) error {
		if current == nil {
			return nil
		}
		additional := bytes.TrimSpace(current.AdditionalProperties)
		if len(additional) > 0 && !bytes.Equal(additional, []byte("false")) && !bytes.Equal(additional, []byte("true")) {
			var child node
			if err := decodeDeclaredSchema(additional, &child); err != nil {
				return err
			}
		}
		children := append([]*node{current.Items}, current.AnyOf...)
		for _, child := range current.Properties {
			children = append(children, child)
		}
		for _, child := range current.Definitions {
			children = append(children, child)
		}
		for _, child := range children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(n)
}

func checkDeclaredSchema(n *node, visited map[*node]bool) error {
	if visited[n] {
		return nil
	}
	visited[n] = true
	if n.Ref == "" && len(n.AnyOf) == 0 {
		typ, nullable := nodeType(n)
		if typ == "object" {
			if len(n.AdditionalProperties) == 0 {
				return fmt.Errorf("objects require explicit false or typed additionalProperties")
			}
			seen := make(map[string]bool)
			for _, key := range n.Required {
				if seen[key] || n.Properties[key] == nil && n.additional == nil {
					return fmt.Errorf("required fields must be unique declared properties")
				}
				seen[key] = true
			}
		} else if len(n.Properties) > 0 || len(n.Required) > 0 || len(n.AdditionalProperties) > 0 {
			return fmt.Errorf("object keywords require an object")
		}
		if typ != "array" && (n.Items != nil || n.MinItems != nil || n.MaxItems != nil) {
			return fmt.Errorf("array keywords require an array")
		}
		if typ != "string" && (n.MinLength != nil || n.MaxLength != nil || n.Pattern != "" || n.Format != "" || n.ContentEncoding != "") {
			return fmt.Errorf("string keywords require a string")
		}
		if typ != "integer" && typ != "number" && (n.Minimum != "" || n.Maximum != "") {
			return fmt.Errorf("numeric bounds require a number or integer")
		}
		for _, pair := range [][2]*int{{n.MinLength, n.MaxLength}, {n.MinItems, n.MaxItems}} {
			if pair[0] != nil && *pair[0] < 0 || pair[1] != nil && *pair[1] < 0 || pair[0] != nil && pair[1] != nil && *pair[0] > *pair[1] {
				return fmt.Errorf("length bounds must be nonnegative and ordered")
			}
		}
		if n.Minimum != "" && n.Maximum != "" {
			lower, _ := new(big.Rat).SetString(n.Minimum.String())
			upper, _ := new(big.Rat).SetString(n.Maximum.String())
			if lower.Cmp(upper) > 0 {
				return fmt.Errorf("minimum exceeds maximum")
			}
		}
		if n.Format != "" && n.Format != "date-time" {
			return fmt.Errorf("unsupported format; use an explicit pattern")
		}
		if n.ContentEncoding != "" && n.ContentEncoding != "base64" {
			return fmt.Errorf("unsupported contentEncoding")
		}
		if len(n.Enum) > 0 && typ != "string" && typ != "integer" && typ != "number" && typ != "boolean" {
			return fmt.Errorf("enum requires a scalar type")
		}
		copy := *n
		copy.Enum, copy.Default = nil, nil
		for _, literal := range append(append([]json.RawMessage(nil), n.Enum...), n.Default) {
			if len(literal) == 0 {
				continue
			}
			var value any
			dec := json.NewDecoder(bytes.NewReader(literal))
			dec.UseNumber()
			if err := dec.Decode(&value); err != nil {
				return fmt.Errorf("invalid enum or default")
			}
			var violations []Violation
			validateValue(&copy, value, "", &violations)
			if len(violations) > 0 {
				return fmt.Errorf("enum or default violates the declared schema")
			}
		}
		if len(n.Default) > 0 && len(n.Enum) > 0 {
			var value any
			dec := json.NewDecoder(bytes.NewReader(n.Default))
			dec.UseNumber()
			if err := dec.Decode(&value); err != nil {
				return fmt.Errorf("invalid default")
			}
			var violations []Violation
			validateValue(n, value, "", &violations)
			if len(violations) > 0 {
				return fmt.Errorf("default must be an enum member")
			}
		}
		// The validator accepts null for nullable types before testing scalar enum
		// membership. Require that explicit schema enums preserve that contract.
		if nullable && len(n.Enum) > 0 {
			found := false
			for _, literal := range n.Enum {
				found = found || bytes.Equal(bytes.TrimSpace(literal), []byte("null"))
			}
			if !found {
				return fmt.Errorf("nullable scalar enums must include null")
			}
		}
	}
	children := append([]*node{n.Items, n.additional, n.reference}, n.AnyOf...)
	for _, child := range n.Properties {
		children = append(children, child)
	}
	for _, child := range n.Definitions {
		children = append(children, child)
	}
	for _, child := range children {
		if child != nil {
			if err := checkDeclaredSchema(child, visited); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *generator) importCustom(n *node) *node {
	keys := make([]string, 0, len(n.Definitions))
	for key := range n.Definitions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	names := make(map[*node]string, len(keys))
	for _, key := range keys {
		entry := &definition{key: fmt.Sprintf("T%d", len(g.definitions)), schema: n.Definitions[key]}
		g.definitions = append(g.definitions, entry)
		names[entry.schema] = entry.key
	}
	var rebase func(*node)
	rebase = func(current *node) {
		if current == nil {
			return
		}
		if current.Ref != "" {
			current.Ref = "#/$defs/" + names[current.reference]
		}
		for _, child := range current.Properties {
			rebase(child)
		}
		for _, child := range current.AnyOf {
			rebase(child)
		}
		rebase(current.Items)
		if current.additional != nil {
			rebase(current.additional)
			current.AdditionalProperties, _ = json.Marshal(current.additional)
		}
	}
	for _, key := range keys {
		rebase(n.Definitions[key])
	}
	n.Definitions = nil
	rebase(n)
	return n
}

func wireNode(n *node) *node {
	for n.reference != nil || len(n.AnyOf) > 0 {
		if n.reference != nil {
			n = n.reference
		} else {
			n = nullableReference(n)
		}
	}
	return n
}

func acceptsNull(n *node) bool {
	for n.reference != nil || len(n.AnyOf) > 0 {
		if len(n.AnyOf) > 0 {
			return true
		}
		n = n.reference
	}
	typ, nullable := nodeType(n)
	return nullable || typ == "null"
}

// ParameterShape reports a scalar or flat scalar-array HTTP representation.
func (v *Validator) ParameterShape() (kind, itemKind string, ok bool) {
	if v == nil {
		return "", "", false
	}
	root := wireNode(&v.root)
	kind, _ = nodeType(root)
	scalar := func(typ string) bool {
		return typ == "string" || typ == "integer" || typ == "number" || typ == "boolean"
	}
	if scalar(kind) {
		return kind, "", true
	}
	if kind == "array" && root.Items != nil {
		itemKind, _ = nodeType(wireNode(root.Items))
		return kind, itemKind, scalar(itemKind)
	}
	return "", "", false
}
