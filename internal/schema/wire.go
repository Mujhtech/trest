package schema

import (
	"bytes"
	"encoding/json"
	"sort"
	"unicode/utf8"
)

// wirePlan avoids building a second object tree for ordinary flat payloads.
// The standard library still validates the complete JSON grammar first. Any
// unknown, duplicated, absent required, or nested value uses the full decoder.
type wirePlan struct {
	root     *node
	scalar   bool
	fields   map[string]wireField
	required uint64
}

type wireField struct {
	node *node
	path string
	bit  uint64
}

func compileWirePlan(root *node) *wirePlan {
	if wireScalar(root) {
		return &wirePlan{root: root, scalar: true}
	}
	typ, nullable := nodeType(root)
	if root.reference != nil || len(root.AnyOf) != 0 || typ != "object" || nullable ||
		root.additional != nil || len(root.Enum) != 0 || len(root.Properties) > 64 {
		return nil
	}
	keys := make([]string, 0, len(root.Properties))
	for key, child := range root.Properties {
		if !wireScalar(child) {
			return nil
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	plan := &wirePlan{root: root, fields: make(map[string]wireField, len(keys))}
	for i, key := range keys {
		plan.fields[key] = wireField{node: root.Properties[key], path: joinPath("", key), bit: uint64(1) << uint(i)}
	}
	for _, key := range root.Required {
		field, ok := plan.fields[key]
		if !ok || plan.required&field.bit != 0 {
			return nil
		}
		plan.required |= field.bit
	}
	return plan
}

func wireScalar(n *node) bool {
	if n == nil || n.reference != nil || len(n.AnyOf) != 0 {
		return false
	}
	typ, _ := nodeType(n)
	switch typ {
	case "string", "integer", "number", "boolean", "null":
		return true
	}
	return false
}

// validate is called only after json.Valid. Returning false discards any partial
// result and delegates the original bytes to the full validator.
func (p *wirePlan) validate(data []byte) ([]Violation, bool) {
	cursor := wireCursor{data: data}
	cursor.skipSpace()
	var violations []Violation
	if p.scalar {
		value, ok := cursor.scalar()
		if !ok {
			return nil, false
		}
		cursor.skipSpace()
		if cursor.position != len(data) {
			return nil, false
		}
		value.validate(p.root, "", &violations)
	} else {
		if !cursor.take('{') {
			return nil, false
		}
		cursor.skipSpace()
		var seen uint64
		if !cursor.take('}') {
			for {
				field, ok := p.field(&cursor)
				if !ok || seen&field.bit != 0 {
					return nil, false
				}
				seen |= field.bit
				cursor.skipSpace()
				if !cursor.take(':') {
					return nil, false
				}
				cursor.skipSpace()
				value, ok := cursor.scalar()
				if !ok {
					return nil, false
				}
				value.validate(field.node, field.path, &violations)
				cursor.skipSpace()
				if cursor.take('}') {
					break
				}
				if !cursor.take(',') {
					return nil, false
				}
				cursor.skipSpace()
			}
		}
		cursor.skipSpace()
		if seen&p.required != p.required || cursor.position != len(data) {
			return nil, false
		}
	}
	if len(violations) > 1 {
		sort.SliceStable(violations, func(i, j int) bool {
			if violations[i].Path == violations[j].Path {
				return violations[i].Code < violations[j].Code
			}
			return violations[i].Path < violations[j].Path
		})
	}
	return violations, true
}

func (p *wirePlan) field(cursor *wireCursor) (wireField, bool) {
	start, end, escaped, ok := cursor.quoted()
	if !ok {
		return wireField{}, false
	}
	if !escaped && utf8.Valid(cursor.data[start:end]) {
		// The temporary string is used only as a map lookup key; it does not
		// retain or alias the caller's request bytes in the compiled plan.
		field, ok := p.fields[string(cursor.data[start:end])]
		return field, ok
	}
	var key string
	if err := json.Unmarshal(cursor.data[start-1:end+1], &key); err != nil {
		return wireField{}, false
	}
	field, ok := p.fields[key]
	return field, ok
}

type wireCursor struct {
	data     []byte
	position int
	// The validator's cursor follows json.Valid; the native decoder instead
	// validates its supported grammar as it parses to avoid a second full scan.
	strict bool
}

func (c *wireCursor) skipSpace() {
	for c.position < len(c.data) {
		switch c.data[c.position] {
		case ' ', '\t', '\r', '\n':
			c.position++
		default:
			return
		}
	}
}

func (c *wireCursor) take(character byte) bool {
	if c.position >= len(c.data) || c.data[c.position] != character {
		return false
	}
	c.position++
	return true
}

// quoted returns the content bounds of a JSON string and consumes its quotes.
// Complete grammar checking remains encoding/json's responsibility.
func (c *wireCursor) quoted() (start, end int, escaped, ok bool) {
	if !c.take('"') {
		return 0, 0, false, false
	}
	start = c.position
	// Most field names and scalar strings have no escapes. Use the standard
	// library's optimized byte search for those spans. Escaped and invalid-UTF8
	// tokens retain encoding/json's grammar checks and normalization below.
	if quote := bytes.IndexByte(c.data[start:], '"'); quote >= 0 {
		end = start + quote
		if bytes.IndexByte(c.data[start:end], '\\') < 0 {
			if c.strict {
				for _, character := range c.data[start:end] {
					if character < 0x20 {
						return 0, 0, false, false
					}
				}
			}
			c.position = end + 1
			return start, end, false, true
		}
	}
	for c.position < len(c.data) {
		switch c.data[c.position] {
		case '"':
			end = c.position
			c.position++
			return start, end, escaped, true
		case '\\':
			escaped = true
			c.position += 2
		default:
			c.position++
		}
	}
	return 0, 0, false, false
}

// Raw scalar bytes live only for the duration of validation. Deferring conversion
// until the validator call avoids heap-backed interface boxes and lets the
// compiler use temporary strings without retaining the caller's input buffer.
type wireValue struct {
	raw  []byte
	text string
	kind byte
}

func (v wireValue) validate(n *node, path string, violations *[]Violation) {
	switch v.kind {
	case 's':
		if typ, _ := nodeType(n); typ == "string" {
			if hasStringChecks(n) {
				validateStringValue(n, string(v.raw), path, violations)
			}
			return
		}
		validateValue(n, string(v.raw), path, violations)
	case 'd':
		if typ, _ := nodeType(n); typ == "string" {
			validateStringValue(n, v.text, path, violations)
			return
		}
		validateValue(n, v.text, path, violations)
	case 'n':
		validateValue(n, json.Number(string(v.raw)), path, violations)
	case 't':
		validateValue(n, true, path, violations)
	case 'f':
		validateValue(n, false, path, violations)
	case '0':
		validateValue(n, nil, path, violations)
	}
}

func (c *wireCursor) scalar() (wireValue, bool) {
	if c.position >= len(c.data) {
		return wireValue{}, false
	}
	switch c.data[c.position] {
	case '"':
		start, end, escaped, ok := c.quoted()
		if !ok {
			return wireValue{}, false
		}
		if !escaped && utf8.Valid(c.data[start:end]) {
			return wireValue{raw: c.data[start:end], kind: 's'}, true
		}
		var value string
		if err := json.Unmarshal(c.data[start-1:end+1], &value); err != nil {
			return wireValue{}, false
		}
		return wireValue{text: value, kind: 'd'}, true
	case 't':
		return wireValue{kind: 't'}, c.literal("true")
	case 'f':
		return wireValue{kind: 'f'}, c.literal("false")
	case 'n':
		return wireValue{kind: '0'}, c.literal("null")
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		start := c.position
		for c.position < len(c.data) {
			switch c.data[c.position] {
			case '-', '+', '.', 'e', 'E', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
				c.position++
			default:
				return wireValue{raw: c.data[start:c.position], kind: 'n'}, true
			}
		}
		return wireValue{raw: c.data[start:c.position], kind: 'n'}, true
	default:
		return wireValue{}, false
	}
}

func (c *wireCursor) literal(text string) bool {
	end := c.position + len(text)
	if end > len(c.data) || c.strict && string(c.data[c.position:end]) != text {
		return false
	}
	c.position = end
	return true
}
