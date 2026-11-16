// Package jsonoutput compiles JSON encoding for closed native Go value shapes.
// Unsupported shapes retain encoding/json through the caller's fallback.
package jsonoutput

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Plan contains immutable encoding instructions. A plan can be shared between
// requests; output storage and values remain owned by each caller.
type Plan struct {
	kind   reflect.Kind
	fields []field
}

type field struct {
	index     int
	name      []byte
	omitEmpty bool
	plan      *Plan
}

// New returns a plan only when a type's complete encoding/json representation
// can be reproduced without invoking application code. Native strings, booleans,
// integers, and nonembedded structs containing those shapes are supported when
// neither the value type nor its pointer has exported methods.
// Pointers, collections, floats, codecs, ambiguous fields, and unsupported JSON
// tag options return nil so the caller can retain its normal JSON encoder.
func New(typ reflect.Type) *Plan { return compile(typ, 0) }

func compile(typ reflect.Type, depth int) *Plan {
	if typ == nil || depth > 128 || typ == reflect.TypeFor[json.Number]() {
		return nil
	}
	// Newer standard encoders recognize additional codec interfaces such as
	// AppendText and MarshalJSONTo. Restricting plans to method-free shapes
	// preserves them without importing APIs newer than the core's Go baseline.
	if typ.NumMethod() != 0 || reflect.PointerTo(typ).NumMethod() != 0 {
		return nil
	}
	plan := &Plan{kind: typ.Kind()}
	switch typ.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return plan
	case reflect.Struct:
		seen := make(map[string]bool, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			member := typ.Field(i)
			tag := member.Tag.Get("json")
			if tag == "-" {
				continue
			}
			// Even unexported anonymous structs can promote exported fields.
			// Leave field dominance and embedding to encoding/json.
			if member.Anonymous {
				return nil
			}
			if member.PkgPath != "" {
				continue
			}
			name, options, hasOptions := strings.Cut(tag, ",")
			if name == "" {
				name = member.Name
			}
			if !validName(name) || seen[name] {
				return nil
			}
			seen[name] = true
			omitEmpty := false
			if hasOptions {
				for _, option := range strings.Split(options, ",") {
					if option != "omitempty" {
						return nil
					}
					omitEmpty = true
				}
			}
			child := compile(member.Type, depth+1)
			if child == nil {
				return nil
			}
			// Marshaling a field name cannot fail. Only immutable type metadata
			// is encoded here; every response value is read during Append.
			encoded, _ := json.Marshal(name)
			plan.fields = append(plan.fields, field{
				index: i, name: append(encoded, ':'),
				// encoding/json's omitempty never omits a struct, even a zero one.
				omitEmpty: omitEmpty && member.Type.Kind() != reflect.Struct,
				plan:      child,
			})
		}
		return plan
	default:
		return nil
	}
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", character) && !unicode.IsLetter(character) && !unicode.IsDigit(character) {
			return false
		}
	}
	return true
}

// Append appends the current value's JSON representation to dst. The caller
// must supply the exact type used to construct this nonnil plan and must not
// mutate the value concurrently. No value or output buffer is retained by Plan.
func (p *Plan) Append(dst []byte, value reflect.Value) []byte {
	switch p.kind {
	case reflect.String:
		return appendString(dst, value.String())
	case reflect.Bool:
		return strconv.AppendBool(dst, value.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(dst, value.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(dst, value.Uint(), 10)
	case reflect.Struct:
		dst = append(dst, '{')
		written := false
		for _, member := range p.fields {
			child := value.Field(member.index)
			if member.omitEmpty && child.IsZero() {
				continue
			}
			if written {
				dst = append(dst, ',')
			}
			written = true
			dst = append(dst, member.name...)
			dst = member.plan.Append(dst, child)
		}
		return append(dst, '}')
	default:
		panic("jsonoutput: invalid compiled plan")
	}
}

// appendString follows encoding/json's default escaping, including HTML-sensitive
// bytes and U+2028/U+2029. Invalid UTF-8 retains the active standard encoder's
// representation, which differs between the original and jsonv2 implementations.
// strconv.AppendQuote uses Go string escapes and cannot preserve this contract.
func appendString(dst []byte, value string) []byte {
	const hex = "0123456789abcdef"
	originalLength := len(dst)
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(value); {
		character := value[i]
		if character < utf8.RuneSelf {
			if character >= 0x20 && character != '\\' && character != '"' && character != '<' && character != '>' && character != '&' {
				i++
				continue
			}
			dst = append(dst, value[start:i]...)
			switch character {
			case '\\', '"':
				dst = append(dst, '\\', character)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hex[character>>4], hex[character&0xf])
			}
			i++
			start = i
			continue
		}
		characterRune, size := utf8.DecodeRuneInString(value[i:])
		if characterRune == utf8.RuneError && size == 1 {
			// A string cannot fail to marshal. Discard only this string's
			// partial output, preserving the caller's prefix and other fields.
			// Clone on this cold path so json.Marshal's retained argument does
			// not force every caller's ordinary output value onto the heap.
			encoded, _ := json.Marshal(strings.Clone(value))
			return append(dst[:originalLength], encoded...)
		}
		if characterRune == '\u2028' || characterRune == '\u2029' {
			dst = append(dst, value[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hex[characterRune&0xf])
			i += size
			start = i
			continue
		}
		i += size
	}
	dst = append(dst, value[start:]...)
	return append(dst, '"')
}
