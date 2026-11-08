// Package schema compiles Go data declarations into deterministic JSON contracts.
package schema

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type node struct {
	Type                 any               `json:"type,omitempty"`
	Ref                  string            `json:"$ref,omitempty"`
	Definitions          map[string]*node  `json:"$defs,omitempty"`
	AnyOf                []*node           `json:"anyOf,omitempty"`
	Properties           map[string]*node  `json:"properties,omitempty"`
	Required             []string          `json:"required,omitempty"`
	AdditionalProperties json.RawMessage   `json:"additionalProperties,omitempty"`
	Items                *node             `json:"items,omitempty"`
	MinItems             *int              `json:"minItems,omitempty"`
	MaxItems             *int              `json:"maxItems,omitempty"`
	Minimum              json.Number       `json:"minimum,omitempty"`
	Maximum              json.Number       `json:"maximum,omitempty"`
	MinLength            *int              `json:"minLength,omitempty"`
	MaxLength            *int              `json:"maxLength,omitempty"`
	Pattern              string            `json:"pattern,omitempty"`
	Enum                 []json.RawMessage `json:"enum,omitempty"`
	Format               string            `json:"format,omitempty"`
	ContentEncoding      string            `json:"contentEncoding,omitempty"`
	Description          string            `json:"description,omitempty"`
	Default              json.RawMessage   `json:"default,omitempty"`
	additional           *node
	compiledPattern      *regexp.Regexp
	reference            *node
	minimumValue         *numberValue
	maximumValue         *numberValue
	enumValues           []enumValue
}

var customInterfaces = []reflect.Type{
	reflect.TypeFor[json.Marshaler](),
	reflect.TypeFor[json.Unmarshaler](),
	reflect.TypeFor[encoding.TextMarshaler](),
	reflect.TypeFor[encoding.TextUnmarshaler](),
}

// Generate returns a deterministic JSON Schema for t, or an error identifying the
// unsupported field. JSON names default to exported Go field names. Declarative
// constraints apply equally to validation and documentation. Recursive types use
// local $defs references. Custom codecs must declare their wire shape through
// Provider; generation does not infer a contract from codec implementation code.
func Generate(t reflect.Type) (json.RawMessage, error) {
	g := generator{active: make(map[reflect.Type]*definition)}
	n, err := g.generate(t, "", 0)
	if err != nil {
		return nil, err
	}
	g.attachDefinitions(n)
	data, err := json.Marshal(n)
	if err != nil {
		return nil, fmt.Errorf("schema: encode: %w", err)
	}
	return data, nil
}

// GenerateField describes one field, including its declarative constraints.
// HTTP bindings use this to share the same rules as JSON request contracts.
func GenerateField(f reflect.StructField) (json.RawMessage, error) {
	g := generator{active: make(map[reflect.Type]*definition)}
	n, err := g.generate(f.Type, "", 0)
	if err != nil {
		return nil, err
	}
	if err := applyTags(n, f, "/"+f.Name); err != nil {
		return nil, err
	}
	g.attachDefinitions(n)
	return json.Marshal(n)
}

type definition struct {
	key    string
	schema *node
}

type generator struct {
	active      map[reflect.Type]*definition
	definitions []*definition
}

func (g *generator) attachDefinitions(n *node) {
	if len(g.definitions) == 0 {
		return
	}
	n.Definitions = make(map[string]*node, len(g.definitions))
	for _, entry := range g.definitions {
		// The document root may itself be recursive. Copy its body before adding
		// definitions so the serialized schema graph remains finite.
		body := *entry.schema
		n.Definitions[entry.key] = &body
	}
}

func (g *generator) generate(t reflect.Type, path string, depth int) (*node, error) {
	if t == nil {
		return nil, unsupported(path, "nil type")
	}
	if entry := g.active[t]; entry != nil {
		if entry.key == "" {
			entry.key = "T" + strconv.Itoa(len(g.definitions))
			g.definitions = append(g.definitions, entry)
		}
		return &node{Ref: "#/$defs/" + entry.key}, nil
	}
	if depth > 128 {
		return nil, unsupported(path, "maximum type nesting exceeded")
	}
	if t == reflect.TypeFor[time.Time]() {
		return &node{Type: "string", Format: "date-time"}, nil
	}
	if t.Kind() == reflect.Pointer {
		entry := &definition{}
		g.active[t] = entry
		defer delete(g.active, t)
		child, err := g.generate(t.Elem(), path, depth+1)
		if err != nil {
			return nil, err
		}
		makeNullable(child)
		entry.schema = child
		copy := *child
		return &copy, nil
	}
	if HasCustomCodec(t) || reflect.PointerTo(t).Implements(reflect.TypeFor[Provider]()) {
		custom, err := customSchema(t, path)
		if err != nil {
			return nil, err
		}
		return g.importCustom(custom), nil
	}
	entry := &definition{}
	g.active[t] = entry
	defer delete(g.active, t)

	n := &node{}
	switch t.Kind() {
	case reflect.Struct:
		n.Type = "object"
		n.Properties = make(map[string]*node)
		n.AdditionalProperties = json.RawMessage("false")
		fields, err := structFields(t, path)
		if err != nil {
			return nil, err
		}
		for _, field := range fields {
			f, name := field.field, field.name
			child, err := g.generate(f.Type, joinPath(path, name), depth+1)
			if err != nil {
				return nil, err
			}
			if err := applyTags(child, f, joinPath(path, name)); err != nil {
				return nil, err
			}
			n.Properties[name] = child
			if !field.optional {
				n.Required = append(n.Required, name)
			}
		}
		sort.Strings(n.Required)
	case reflect.String:
		n.Type = "string"
	case reflect.Bool:
		n.Type = "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n.Type = "integer"
		bits := t.Bits()
		n.Minimum = json.Number(strconv.FormatInt(-1<<(bits-1), 10))
		n.Maximum = json.Number(strconv.FormatInt(1<<(bits-1)-1, 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n.Type = "integer"
		n.Minimum = "0"
		n.Maximum = json.Number(strconv.FormatUint(1<<t.Bits()-1, 10))
	case reflect.Float32:
		n.Type, n.Minimum, n.Maximum = "number", "-3.4028234663852886e+38", "3.4028234663852886e+38"
	case reflect.Float64:
		n.Type, n.Minimum, n.Maximum = "number", "-1.7976931348623157e+308", "1.7976931348623157e+308"
	case reflect.Array, reflect.Slice:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			n.Type = "string"
			n.ContentEncoding = "base64"
			makeNullable(n)
			return n, nil
		}
		n.Type = "array"
		child, err := g.generate(t.Elem(), joinPath(path, "*"), depth+1)
		if err != nil {
			return nil, err
		}
		n.Items = child
		if t.Kind() == reflect.Array {
			length := t.Len()
			n.MinItems, n.MaxItems = &length, &length
		} else {
			makeNullable(n)
		}
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, unsupported(path, "map keys must be strings")
		}
		for _, codec := range customInterfaces {
			if t.Key().Implements(codec) || reflect.PointerTo(t.Key()).Implements(codec) {
				return nil, unsupported(path, "custom map key codec")
			}
		}
		child, err := g.generate(t.Elem(), joinPath(path, "*"), depth+1)
		if err != nil {
			return nil, err
		}
		n.Type = "object"
		n.AdditionalProperties, err = json.Marshal(child)
		if err != nil {
			return nil, err
		}
		makeNullable(n)
	default:
		return nil, unsupported(path, "unsupported type "+t.String())
	}
	entry.schema = n
	copy := *n
	return &copy, nil
}

func makeNullable(n *node) {
	if n.Ref != "" {
		ref := *n
		*n = node{AnyOf: []*node{&ref, {Type: "null"}}}
		return
	}
	if typ, ok := n.Type.(string); ok {
		if typ != "null" {
			n.Type = []string{typ, "null"}
		}
	}
	if len(n.Enum) > 0 {
		for _, member := range n.Enum {
			if strings.TrimSpace(string(member)) == "null" {
				return
			}
		}
		n.Enum = append(n.Enum, json.RawMessage("null"))
	}
}

func validName(s string) bool {
	if s == "" || s == "-" {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c) && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

func unsupported(path, reason string) error {
	if path == "" {
		path = "/"
	}
	return fmt.Errorf("schema: %s: %s", path, reason)
}

func joinPath(path, key string) string {
	return path + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
