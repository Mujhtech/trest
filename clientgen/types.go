package clientgen

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func (d *document) tsType(s *shape) (string, error) {
	if ref, nullable := s.reference(); ref != "" {
		name, err := refName(ref)
		typ := "Schema" + goName(name)
		if nullable {
			typ = "(" + typ + ") | null"
		}
		return typ, err
	}
	typ, nullable := s.kind()
	var result string
	switch typ {
	case "":
		result = "unknown"
	case "null":
		return "null", nil
	case "string":
		result = "string"
	case "integer", "number":
		result = "number"
	case "boolean":
		result = "boolean"
	case "array":
		child, err := d.tsType(s.Items)
		if err != nil {
			return "", err
		}
		result = "Array<" + child + ">"
	case "object":
		if len(s.Additional) > 0 && string(s.Additional) != "false" {
			child := "unknown"
			if string(s.Additional) != "true" {
				var c shape
				if err := json.Unmarshal(s.Additional, &c); err != nil {
					return "", err
				}
				var err error
				child, err = d.tsType(&c)
				if err != nil {
					return "", err
				}
			}
			result = "Record<string, " + child + ">"
		} else {
			var b strings.Builder
			b.WriteString("{ ")
			for _, name := range keys(s.Properties) {
				child, err := d.tsType(s.Properties[name])
				if err != nil {
					return "", err
				}
				b.WriteString(jsQuote(name))
				if !required(s, name) {
					b.WriteByte('?')
				}
				b.WriteString(": " + child + "; ")
			}
			b.WriteByte('}')
			result = b.String()
		}
	default:
		return "", fmt.Errorf("unsupported type %q", typ)
	}
	if len(s.Enum) > 0 && (typ == "string" || typ == "integer" || typ == "number" || typ == "boolean") {
		values := make([]string, 0, len(s.Enum))
		for _, v := range s.Enum {
			encoded, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			values = append(values, string(encoded))
		}
		result = strings.Join(values, " | ")
	}
	if nullable {
		result = "(" + result + ") | null"
	}
	return result, nil
}
func (d *document) goType(s *shape) (string, error) {
	if ref, nullable := s.reference(); ref != "" {
		name, err := refName(ref)
		typ := "Schema" + goName(name)
		if nullable {
			typ = pointer(typ)
		}
		return typ, err
	}
	typ, nullable := s.kind()
	var result string
	switch typ {
	case "", "null":
		return "json.RawMessage", nil
	case "string":
		result = "string"
	case "integer":
		result = "int64"
		if s.Minimum == "0" && s.Maximum == "18446744073709551615" {
			result = "uint64"
		}
	case "number":
		result = "float64"
	case "boolean":
		result = "bool"
	case "array":
		child, err := d.goType(s.Items)
		if err != nil {
			return "", err
		}
		result = "[]" + child
	case "object":
		if len(s.Additional) > 0 && string(s.Additional) != "false" {
			child := "json.RawMessage"
			if string(s.Additional) != "true" {
				var c shape
				if err := json.Unmarshal(s.Additional, &c); err != nil {
					return "", err
				}
				var err error
				child, err = d.goType(&c)
				if err != nil {
					return "", err
				}
			}
			result = "map[string]" + child
		} else {
			var b strings.Builder
			b.WriteString("struct {\n")
			for _, name := range keys(s.Properties) {
				child, err := d.goType(s.Properties[name])
				if err != nil {
					return "", err
				}
				tag := name
				if !required(s, name) {
					child = pointer(child)
					tag += ",omitempty"
				}
				fmt.Fprintf(&b, "%s %s %s\n", goName(name), child, strconv.Quote("json:"+strconv.Quote(tag)))
			}
			b.WriteString("}")
			result = b.String()
		}
	default:
		return "", fmt.Errorf("unsupported type %q", typ)
	}
	if nullable {
		result = pointer(result)
	}
	return result, nil
}
func pointer(typ string) string {
	if strings.HasPrefix(typ, "*") {
		return typ
	}
	return "*" + typ
}
func parameters(op operation, where string) []parameter {
	out := []parameter{}
	for _, p := range op.Parameters {
		if p.In == where {
			out = append(out, p)
		}
	}
	return out
}
func groupRequired(params []parameter) bool {
	for _, p := range params {
		if p.Required {
			return true
		}
	}
	return false
}
func inputRequired(op operation) bool {
	if op.Body != nil && op.Body.Required {
		return true
	}
	for _, p := range op.Parameters {
		if p.Required && p.In != "cookie" {
			return true
		}
	}
	return false
}

func jsQuote(value string) string { data, _ := json.Marshal(value); return string(data) }
