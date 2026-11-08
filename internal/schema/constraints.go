package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

func applyTags(n *node, field reflect.StructField, path string) error {
	typ, nullable := nodeType(n)
	for _, entry := range []struct {
		tag    string
		target *json.Number
		lower  bool
	}{
		{"min", &n.Minimum, true}, {"max", &n.Maximum, false},
	} {
		text, exists := field.Tag.Lookup(entry.tag)
		if !exists {
			continue
		}
		if typ != "integer" && typ != "number" {
			return unsupported(path, entry.tag+" requires a numeric field")
		}
		value, err := numericTag(text, typ)
		if err != nil {
			return unsupported(path, entry.tag+": "+err.Error())
		}
		v, _ := new(big.Rat).SetString(value.String())
		if *entry.target != "" {
			bound, _ := new(big.Rat).SetString(entry.target.String())
			if (entry.lower && v.Cmp(bound) < 0) || (!entry.lower && v.Cmp(bound) > 0) {
				return unsupported(path, entry.tag+" exceeds the Go type's range")
			}
		}
		*entry.target = value
	}
	if n.Minimum != "" && n.Maximum != "" {
		min, _ := new(big.Rat).SetString(n.Minimum.String())
		max, _ := new(big.Rat).SetString(n.Maximum.String())
		if min.Cmp(max) > 0 {
			return unsupported(path, "min exceeds max")
		}
	}
	for _, entry := range []struct {
		tag, kind string
		target    **int
	}{
		{"minLength", "string", &n.MinLength}, {"maxLength", "string", &n.MaxLength},
		{"minItems", "array", &n.MinItems}, {"maxItems", "array", &n.MaxItems},
	} {
		text, exists := field.Tag.Lookup(entry.tag)
		if !exists {
			continue
		}
		if typ != entry.kind {
			return unsupported(path, entry.tag+" is incompatible with field type")
		}
		v, err := strconv.Atoi(text)
		if err != nil || v < 0 {
			return unsupported(path, entry.tag+" requires a nonnegative integer")
		}
		if typ == "array" && field.Type.Kind() == reflect.Array {
			if entry.tag == "minItems" && v > field.Type.Len() || entry.tag == "maxItems" && v < field.Type.Len() {
				return unsupported(path, "array length contradicts constraint")
			}
			continue
		}
		if bound := *entry.target; bound != nil {
			if strings.HasPrefix(entry.tag, "min") && v < *bound || strings.HasPrefix(entry.tag, "max") && v > *bound {
				return unsupported(path, entry.tag+" cannot weaken the declared schema")
			}
		}
		*entry.target = &v
	}
	if n.MinLength != nil && n.MaxLength != nil && *n.MinLength > *n.MaxLength {
		return unsupported(path, "minLength exceeds maxLength")
	}
	if n.MinItems != nil && n.MaxItems != nil && *n.MinItems > *n.MaxItems {
		return unsupported(path, "minItems exceeds maxItems")
	}
	if pattern, exists := field.Tag.Lookup("pattern"); exists {
		if typ != "string" {
			return unsupported(path, "pattern requires a string field")
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return unsupported(path, "pattern is not a valid Go regular expression")
		}
		if n.Pattern != "" && n.Pattern != pattern {
			return unsupported(path, "pattern cannot replace the declared schema pattern")
		}
		n.Pattern = pattern
	}
	if values, exists := field.Tag.Lookup("enum"); exists {
		if typ != "string" && typ != "integer" && typ != "number" && typ != "boolean" {
			return unsupported(path, "enum requires a scalar field")
		}
		declaredEnum := n.Enum
		n.Enum = nil
		seen := make(map[string]bool)
		for _, text := range strings.Split(values, ",") {
			encoded, err := literalForNode(n, text)
			if err != nil {
				return unsupported(path, "invalid enum value")
			}
			if seen[string(encoded)] {
				return unsupported(path, "duplicate enum value")
			}
			if len(declaredEnum) > 0 {
				found := false
				for _, member := range declaredEnum {
					found = found || bytes.Equal(member, encoded)
				}
				if !found {
					return unsupported(path, "enum cannot extend the declared schema choices")
				}
			}
			seen[string(encoded)] = true
			n.Enum = append(n.Enum, encoded)
		}
		if nullable {
			n.Enum = append(n.Enum, json.RawMessage("null"))
		}
	}
	if description, exists := field.Tag.Lookup("description"); exists {
		n.Description = description
	}
	if text, exists := field.Tag.Lookup("default"); exists {
		encoded, err := literalForNode(n, text)
		if err != nil {
			return unsupported(path, "invalid default value")
		}
		n.Default = encoded
	}
	if len(n.Enum) == 0 && len(n.Default) == 0 {
		return nil
	}
	// Validate declared defaults and enum members against the other constraints.
	copyNode := *n
	copyNode.Enum = nil
	copyNode.Default = nil
	if err := compileNode(&copyNode, nil, 0); err != nil {
		return unsupported(path, err.Error())
	}
	for _, encoded := range append(append([]json.RawMessage(nil), n.Enum...), n.Default) {
		if len(encoded) == 0 {
			continue
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return unsupported(path, "invalid constraint value")
		}
		var violations []Violation
		validateValue(&copyNode, value, "", &violations)
		if len(violations) > 0 {
			return unsupported(path, "constraint value violates field schema")
		}
	}
	if len(n.Default) > 0 && len(n.Enum) > 0 {
		matches := false
		for _, member := range n.Enum {
			if bytes.Equal(n.Default, member) {
				matches = true
				break
			}
		}
		if !matches {
			return unsupported(path, "default must be an enum member")
		}
	}
	return nil
}

func numericTag(text, typ string) (json.Number, error) {
	if len(text) > 1024 || text == "" {
		return "", fmt.Errorf("invalid numeric constraint")
	}
	if typ == "integer" && strings.ContainsAny(text, ".eE") {
		return "", fmt.Errorf("integer constraint must use integer notation")
	}
	var number json.Number
	if err := json.Unmarshal([]byte(text), &number); err != nil {
		return "", fmt.Errorf("invalid numeric constraint")
	}
	if i := strings.IndexAny(text, "eE"); i >= 0 {
		exp, err := strconv.Atoi(text[i+1:])
		if err != nil || exp < -1000 || exp > 1000 {
			return "", fmt.Errorf("numeric exponent out of range")
		}
	}
	if _, ok := new(big.Rat).SetString(text); !ok {
		return "", fmt.Errorf("invalid numeric constraint")
	}
	if typ == "number" {
		v, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return "", fmt.Errorf("nonfinite numeric constraint")
		}
	}
	return number, nil
}

func literalForNode(n *node, text string) (json.RawMessage, error) {
	typ, _ := nodeType(n)
	switch typ {
	case "string":
		return json.Marshal(text)
	case "integer", "number":
		v, err := numericTag(text, typ)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(v), nil
	case "boolean":
		if text != "true" && text != "false" {
			return nil, fmt.Errorf("invalid boolean")
		}
		return json.RawMessage(text), nil
	default:
		return nil, fmt.Errorf("default requires a scalar field")
	}
}
