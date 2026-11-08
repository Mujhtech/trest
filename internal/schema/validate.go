package schema

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Violation describes a structural failure at a JSON Pointer path. An empty path
// denotes the document root. Codes are stable; messages contain no input values.
type Violation struct {
	Path    string
	Code    string
	Message string
}

// Validate checks data against a schema emitted by Generate. It rejects unknown
// and duplicate object fields, missing required fields, nulls for nonnullable
// values, and numbers outside the represented Go type's range. Malformed JSON
// produces an error; otherwise all structural violations are returned.
func Validate(schema json.RawMessage, data []byte) ([]Violation, error) {
	v, err := Compile(schema)
	if err != nil {
		return nil, err
	}
	return v.Validate(data)
}

// Validator holds a parsed immutable schema. It is safe for concurrent use.
type Validator struct {
	root node
	wire *wirePlan
}

// Compile checks and prepares a schema once, including its regular expressions.
func Compile(spec json.RawMessage) (*Validator, error) {
	v := &Validator{}
	if err := json.Unmarshal(spec, &v.root); err != nil {
		return nil, fmt.Errorf("schema: invalid schema: %w", err)
	}
	if err := compileNode(&v.root, v.root.Definitions, 0); err != nil {
		return nil, fmt.Errorf("schema: invalid schema: %w", err)
	}
	keys := make([]string, 0, len(v.root.Definitions))
	for key := range v.root.Definitions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := compileNode(v.root.Definitions[key], v.root.Definitions, 1); err != nil {
			return nil, fmt.Errorf("schema: invalid definition %q: %w", key, err)
		}
	}
	if err := checkReferences(&v.root, make(map[*node]bool)); err != nil {
		return nil, fmt.Errorf("schema: invalid schema: %w", err)
	}
	v.wire = compileWirePlan(&v.root)
	return v, nil
}

func compileNode(n *node, definitions map[string]*node, depth int) error {
	if n == nil || depth > 128 {
		return fmt.Errorf("invalid schema nesting")
	}
	if depth > 0 && len(n.Definitions) > 0 {
		return fmt.Errorf("definitions must be declared at the schema root")
	}
	if n.Ref != "" {
		if schemaHasConstraints(n) || len(n.AnyOf) > 0 {
			return fmt.Errorf("reference siblings cannot change validation")
		}
		const prefix = "#/$defs/"
		if !strings.HasPrefix(n.Ref, prefix) {
			return fmt.Errorf("only local definition references are supported")
		}
		key := strings.TrimPrefix(n.Ref, prefix)
		if strings.Contains(key, "/") || strings.Contains(strings.ReplaceAll(strings.ReplaceAll(key, "~1", ""), "~0", ""), "~") {
			return fmt.Errorf("invalid definition reference")
		}
		key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
		n.reference = definitions[key]
		if n.reference == nil {
			return fmt.Errorf("unresolved definition reference")
		}
		return nil
	}
	if len(n.AnyOf) > 0 {
		if schemaHasConstraints(n) || len(n.AnyOf) != 2 || nullableReference(n) == nil {
			return fmt.Errorf("only nullable reference unions are supported")
		}
		for _, child := range n.AnyOf {
			if err := compileNode(child, definitions, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	typ, err := checkedType(n)
	if err != nil {
		return err
	}
	if n.Pattern != "" {
		pattern, err := regexp.Compile(n.Pattern)
		if err != nil {
			return fmt.Errorf("invalid pattern")
		}
		n.compiledPattern = pattern
	}
	n.minimumValue, n.maximumValue, n.enumValues = nil, nil, nil
	for _, bound := range []struct {
		text  json.Number
		value **numberValue
	}{{n.Minimum, &n.minimumValue}, {n.Maximum, &n.maximumValue}} {
		if bound.text != "" {
			if _, err := numericTag(bound.text.String(), typ); err != nil {
				return err
			}
			value, _ := parseNumberValue(bound.text.String())
			// Cached bounds are immutable. Keep their rational form too so a
			// noninteger or large input can compare without rebuilding the bound.
			value.asRational()
			*bound.value = &value
		}
	}
	for _, encoded := range n.Enum {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			continue
		}
		member := enumValue{scalar: value}
		if number, ok := value.(json.Number); ok {
			// Preserve enum's finite-number restrictions independently of the
			// node type, including integer schemas with unusually large enums.
			if _, err := numericTag(number.String(), "number"); err == nil {
				parsed, _ := parseNumberValue(number.String())
				parsed.asRational()
				member.number = &parsed
			}
		}
		n.enumValues = append(n.enumValues, member)
	}
	if n.Items != nil {
		if err := compileNode(n.Items, definitions, depth+1); err != nil {
			return err
		}
	} else if typ == "array" {
		return fmt.Errorf("array items required")
	}
	keys := make([]string, 0, len(n.Properties))
	for key := range n.Properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := compileNode(n.Properties[key], definitions, depth+1); err != nil {
			return err
		}
	}
	if len(n.AdditionalProperties) > 0 && !bytes.Equal(n.AdditionalProperties, []byte("false")) {
		if bytes.Equal(n.AdditionalProperties, []byte("true")) {
			return fmt.Errorf("untyped additional properties unsupported")
		}
		var child node
		if err := json.Unmarshal(n.AdditionalProperties, &child); err != nil {
			return err
		}
		if err := compileNode(&child, definitions, depth+1); err != nil {
			return err
		}
		n.additional = &child
	}
	return nil
}

func schemaHasConstraints(n *node) bool {
	return n.Type != nil || len(n.Properties) > 0 || len(n.Required) > 0 || len(n.AdditionalProperties) > 0 || n.Items != nil ||
		n.MinItems != nil || n.MaxItems != nil || n.Minimum != "" || n.Maximum != "" || n.MinLength != nil || n.MaxLength != nil ||
		n.Pattern != "" || len(n.Enum) > 0 || n.Format != "" || n.ContentEncoding != ""
}

func checkedType(n *node) (string, error) {
	typ, _ := nodeType(n)
	valid := func(value string) bool {
		switch value {
		case "object", "array", "string", "integer", "number", "boolean", "null":
			return true
		}
		return false
	}
	if name, ok := n.Type.(string); ok && valid(name) {
		return typ, nil
	}
	var names []string
	switch values := n.Type.(type) {
	case []string:
		names = values
	case []any:
		for _, value := range values {
			name, ok := value.(string)
			if !ok {
				return "", fmt.Errorf("invalid schema type")
			}
			names = append(names, name)
		}
	}
	if len(names) == 2 && names[0] != names[1] && valid(names[0]) && valid(names[1]) && (names[0] == "null" || names[1] == "null") {
		return typ, nil
	}
	return "", fmt.Errorf("unsupported schema type")
}

// nullableReference recognizes the only union emitted by Generate. Keeping the
// union shape narrow makes recursive validation bounded and unambiguous.
func nullableReference(n *node) *node {
	if len(n.AnyOf) != 2 || n.AnyOf[0] == nil || n.AnyOf[1] == nil {
		return nil
	}
	for i, child := range n.AnyOf {
		if child.Type == "null" && child.Ref == "" && len(child.AnyOf) == 0 {
			plainNull := *child
			plainNull.Type = nil
			if schemaHasConstraints(&plainNull) {
				return nil
			}
			other := n.AnyOf[1-i]
			if other.Ref != "" {
				return other
			}
		}
	}
	return nil
}

// checkReferences rejects cycles that never consume a JSON object or array.
// Productive recursion is safe because readValue limits input nesting to 128.
func checkReferences(n *node, visited map[*node]bool) error {
	if visited[n] {
		return nil
	}
	visited[n] = true
	current := n
	seen := make(map[*node]bool)
	for current.reference != nil || len(current.AnyOf) > 0 {
		if seen[current] || len(seen) > 128 {
			return fmt.Errorf("cyclic or excessively deep reference chain")
		}
		seen[current] = true
		if current.reference != nil {
			current = current.reference
		} else {
			current = nullableReference(current)
		}
	}
	children := []*node{n.Items, n.additional, n.reference}
	children = append(children, n.AnyOf...)
	for _, child := range n.Properties {
		children = append(children, child)
	}
	for _, child := range n.Definitions {
		children = append(children, child)
	}
	for _, child := range children {
		if child != nil {
			if err := checkReferences(child, visited); err != nil {
				return err
			}
		}
	}
	return nil
}

// Validate checks one JSON value without mutating the compiled schema.
func (v *Validator) Validate(data []byte) ([]Violation, error) {
	if v == nil {
		return nil, fmt.Errorf("schema: nil validator")
	}
	if v.wire != nil && json.Valid(data) {
		if violations, ok := v.wire.validate(data); ok {
			return violations, nil
		}
	}
	return v.validateSlow(data)
}

// validateSlow preserves the complete decoder's behavior for complex schemas,
// malformed data, and exceptional object shapes. Fast-path tests compare both.
func (v *Validator) validateSlow(data []byte) ([]Violation, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var violations []Violation
	value, err := readValue(dec, "", 0, &violations)
	if err != nil {
		return nil, fmt.Errorf("schema: invalid json: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple json values")
		}
		return nil, fmt.Errorf("schema: trailing json: %w", err)
	}
	validateValue(&v.root, value, "", &violations)
	sort.SliceStable(violations, func(i, j int) bool {
		if violations[i].Path == violations[j].Path {
			return violations[i].Code < violations[j].Code
		}
		return violations[i].Path < violations[j].Path
	})
	return violations, nil
}

func readValue(dec *json.Decoder, path string, depth int, violations *[]Violation) (any, error) {
	if depth > 128 {
		return nil, fmt.Errorf("maximum json nesting exceeded")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		value := make(map[string]any)
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key must be a string")
			}
			childPath := joinPath(path, key)
			if _, exists := value[key]; exists {
				*violations = append(*violations, Violation{childPath, "duplicate_field", "field must appear only once"})
			}
			child, err := readValue(dec, childPath, depth+1, violations)
			if err != nil {
				return nil, err
			}
			value[key] = child
		}
		_, err := dec.Token()
		return value, err
	case '[':
		value := make([]any, 0)
		for dec.More() {
			child, err := readValue(dec, joinPath(path, strconv.Itoa(len(value))), depth+1, violations)
			if err != nil {
				return nil, err
			}
			value = append(value, child)
		}
		_, err := dec.Token()
		return value, err
	default:
		return nil, fmt.Errorf("unexpected json delimiter")
	}
}

func validateValue(n *node, value any, path string, violations *[]Violation) {
	add := func(code, message string) {
		*violations = append(*violations, Violation{path, code, message})
	}
	for n.reference != nil || len(n.AnyOf) > 0 {
		if n.reference != nil {
			n = n.reference
			continue
		}
		if value == nil {
			return
		}
		n = nullableReference(n)
	}
	typ, nullable := nodeType(n)
	if value == nil {
		if !nullable && typ != "null" {
			add("type", "value must not be null")
		}
		return
	}
	if len(n.Enum) > 0 {
		if !matchesEnum(n.enumValues, value) {
			add("enum", "value must be an allowed choice")
		}
	}
	switch typ {
	case "null":
		add("type", "value must be null")
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			add("type", "value must be an object")
			return
		}
		for _, key := range n.Required {
			if _, exists := object[key]; !exists {
				*violations = append(*violations, Violation{joinPath(path, key), "required", "field is required"})
			}
		}
		for key, child := range object {
			childSchema, known := n.Properties[key]
			if !known {
				if n.additional != nil {
					validateValue(n.additional, child, joinPath(path, key), violations)
					continue
				}
				*violations = append(*violations, Violation{joinPath(path, key), "unknown_field", "field is not allowed"})
				continue
			}
			validateValue(childSchema, child, joinPath(path, key), violations)
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			add("type", "value must be an array")
			return
		}
		if (n.MinItems != nil && len(array) < *n.MinItems) || (n.MaxItems != nil && len(array) > *n.MaxItems) {
			add("length", "array has an invalid length")
		}
		for i, child := range array {
			validateValue(n.Items, child, joinPath(path, strconv.Itoa(i)), violations)
		}
	case "string":
		str, ok := value.(string)
		if !ok {
			add("type", "value must be a string")
			return
		}
		length := utf8.RuneCountInString(str)
		if n.MinLength != nil && length < *n.MinLength || n.MaxLength != nil && length > *n.MaxLength {
			add("length", "string has an invalid length")
		}
		if n.compiledPattern != nil && !n.compiledPattern.MatchString(str) {
			add("pattern", "string does not match the required pattern")
		}
		if n.Format == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, str); err != nil {
				add("format", "value must be an RFC3339 timestamp")
			}
		}
		if n.ContentEncoding == "base64" {
			if _, err := base64.StdEncoding.Strict().DecodeString(str); err != nil {
				add("format", "value must be base64 encoded")
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			add("type", "value must be a boolean")
		}
	case "integer", "number":
		number, ok := value.(json.Number)
		if !ok {
			add("type", "value must be a "+typ)
			return
		}
		// encoding/json cannot unmarshal exponent or decimal notation into Go integers.
		if typ == "integer" && strings.ContainsAny(number.String(), ".eE") {
			add("type", "integer must use integer notation")
			return
		}
		// Bound numeric text before exact comparison; extreme exponents must not
		// allocate unbounded big integers. Float parsing safely rejects them.
		if typ == "number" {
			if _, err := strconv.ParseFloat(number.String(), 64); err != nil {
				add("range", "number is outside the supported range")
				return
			}
		}
		if len(number.String()) > 1024 {
			add("range", "number is outside the supported range")
			return
		}
		// A huge negative exponent parses to zero and could grow big.Rat. Reject
		// outlying exponent notation before constructing the exact rational.
		if i := strings.IndexAny(number.String(), "eE"); i >= 0 {
			exponent, err := strconv.ParseInt(number.String()[i+1:], 10, 32)
			if err != nil || exponent < -1000 || exponent > 1000 {
				add("range", "number is outside the supported range")
				return
			}
		}
		v, ok := parseNumberValue(number.String())
		if !ok {
			add("type", "invalid number")
			return
		}
		if n.minimumValue != nil {
			if compareNumber(&v, n.minimumValue) < 0 {
				add("range", "number is below the supported minimum")
			}
		}
		if n.maximumValue != nil {
			if compareNumber(&v, n.maximumValue) > 0 {
				add("range", "number exceeds the supported maximum")
			}
		}
	default:
		add("type", "unsupported schema type")
	}
}

func nodeType(n *node) (string, bool) {
	if typ, ok := n.Type.(string); ok {
		return typ, false
	}
	if types, ok := n.Type.([]string); ok {
		var typ string
		var nullable bool
		for _, value := range types {
			if value == "null" {
				nullable = true
			} else {
				typ = value
			}
		}
		return typ, nullable
	}
	if types, ok := n.Type.([]any); ok {
		var typ string
		var nullable bool
		for _, value := range types {
			if value == "null" {
				nullable = true
			} else if name, ok := value.(string); ok {
				typ = name
			}
		}
		return typ, nullable
	}
	return "", false
}

// numberValue represents common signed and unsigned integers without heap-backed
// arithmetic. A rational is retained for compiled bounds/enums and constructed
// lazily for unusual input values. Per-request values never mutate cached values.
type numberValue struct {
	magnitude uint64
	negative  bool
	integer   bool
	rational  *big.Rat
}

type enumValue struct {
	scalar any
	number *numberValue
}

// text must be a parsed JSON number whose length and exponent were bounded by
// the caller. Integer notation remains distinct from numeric equality: callers
// reject decimal/exponent notation separately for integer schema nodes.
func parseNumberValue(text string) (numberValue, bool) {
	if text == "" {
		return numberValue{}, false
	}
	if !strings.ContainsAny(text, ".eE") {
		digits, negative := text, text[0] == '-'
		if negative {
			digits = text[1:]
		}
		if magnitude, err := strconv.ParseUint(digits, 10, 64); err == nil {
			return numberValue{magnitude: magnitude, negative: negative && magnitude != 0, integer: true}, true
		}
	}
	rational, ok := new(big.Rat).SetString(text)
	return numberValue{rational: rational}, ok
}

func (n *numberValue) asRational() *big.Rat {
	if n.rational == nil {
		n.rational = new(big.Rat).SetUint64(n.magnitude)
		if n.negative {
			n.rational.Neg(n.rational)
		}
	}
	return n.rational
}

// bound is immutable and already has a rational form from compilation.
func compareNumber(value, bound *numberValue) int {
	if value.integer && bound.integer {
		if value.negative != bound.negative {
			if value.negative {
				return -1
			}
			return 1
		}
		comparison := 0
		if value.magnitude < bound.magnitude {
			comparison = -1
		} else if value.magnitude > bound.magnitude {
			comparison = 1
		}
		if value.negative {
			return -comparison
		}
		return comparison
	}
	return value.asRational().Cmp(bound.rational)
}

func matchesEnum(members []enumValue, value any) bool {
	if number, ok := value.(json.Number); ok {
		text := number.String()
		if len(text) > 1024 {
			return false
		}
		if _, err := strconv.ParseFloat(text, 64); err != nil {
			return false
		}
		if i := strings.IndexAny(text, "eE"); i >= 0 {
			exponent, err := strconv.ParseInt(text[i+1:], 10, 32)
			if err != nil || exponent < -1000 || exponent > 1000 {
				return false
			}
		}
		parsed, ok := parseNumberValue(text)
		if !ok {
			return false
		}
		for _, member := range members {
			if member.number != nil && compareNumber(&parsed, member.number) == 0 {
				return true
			}
		}
		return false
	}
	for _, member := range members {
		if equalScalar(value, member.scalar) {
			return true
		}
	}
	return false
}

func equalScalar(a, b any) bool {
	if an, ok := a.(json.Number); ok {
		bn, ok := b.(json.Number)
		if !ok || len(an) > 1024 || len(bn) > 1024 {
			return false
		}
		// Avoid exponent-driven allocations before exact numeric comparison.
		if _, err := numericTag(an.String(), "number"); err != nil {
			return false
		}
		if _, err := numericTag(bn.String(), "number"); err != nil {
			return false
		}
		ar, _ := new(big.Rat).SetString(an.String())
		br, _ := new(big.Rat).SetString(bn.String())
		return ar.Cmp(br) == 0
	}
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}
	return false
}
