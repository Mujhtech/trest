package schema

import (
	"math/bits"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ValueDecoder combines wire validation and assignment for closed native scalar
// objects. It uses the same wire plan and constraints as Validator.Validate;
// Its strict cursor checks the complete grammar while assigning supported native
// values. Unsupported types and inputs use the caller's existing validator and
// encoding/json path.
type ValueDecoder struct {
	typ         reflect.Type
	wire        *wirePlan
	fields      [64]int
	alwaysValid bool
}

// DecoderForType returns a plan only when native assignment exactly matches
// encoding/json. Custom codecs, pointers, embedded/recursive models, containers,
// floats and encoding-sensitive tags deliberately keep the general decoder.
func (v *Validator) DecoderForType(t reflect.Type) *ValueDecoder {
	if v == nil || v.wire == nil || t == nil {
		return nil
	}
	typed := v.ForType(t)
	if typed.plan == nil {
		return nil
	}
	// This whole-type proof covers only value constraints. Parsing must still
	// establish every field's kind, integer notation/range, and wire presence.
	d := &ValueDecoder{typ: t, wire: v.wire, alwaysValid: typed.AlwaysValid()}
	if v.wire.scalar {
		return d
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" || f.Tag.Get("json") == "-" {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		// Wire plans assign one bit per field, at most 64. Index that bit
		// directly instead of hashing it again after the field-name lookup.
		d.fields[bits.TrailingZeros64(v.wire.fields[name].bit)] = i
	}
	return d
}

// TryDecode returns true only after complete grammar, wire constraints, required
// fields and native assignment succeed. target must be a zeroed, settable value
// of the compiled type. On false it may have been partially filled: the caller
// must clear/discard it and use the general path, which owns error diagnostics.
// Strings own their contents; no assigned value aliases the request bytes.
func (d *ValueDecoder) TryDecode(data []byte, target reflect.Value) bool {
	if d == nil || !target.IsValid() || !target.CanSet() || target.Type() != d.typ {
		return false
	}
	cursor := wireCursor{data: data, strict: true}
	cursor.skipSpace()
	if d.wire.scalar {
		value, ok := cursor.scalar()
		if !ok || !decodeScalar(value, d.wire.root, target, !d.alwaysValid) {
			return false
		}
		cursor.skipSpace()
		return cursor.position == len(data)
	}
	if !cursor.take('{') {
		return false
	}
	cursor.skipSpace()
	var seen uint64
	if !cursor.take('}') {
		for {
			field, ok := d.wire.field(&cursor)
			if !ok || seen&field.bit != 0 {
				return false
			}
			seen |= field.bit
			cursor.skipSpace()
			if !cursor.take(':') {
				return false
			}
			cursor.skipSpace()
			value, ok := cursor.scalar()
			if !ok || !decodeScalar(value, field.node, target.Field(d.fields[bits.TrailingZeros64(field.bit)]), !d.alwaysValid) {
				return false
			}
			cursor.skipSpace()
			if cursor.take('}') {
				break
			}
			if !cursor.take(',') {
				return false
			}
			cursor.skipSpace()
		}
	}
	cursor.skipSpace()
	return seen&d.wire.required == d.wire.required && cursor.position == len(data)
}

// TryDecodeParameter validates and assigns one native HTTP scalar without an
// intermediate JSON encoding. Integer and boolean spelling matches the JSON
// parameter syntax; invalid UTF-8 falls back so encoding/json owns replacement.
// A false result leaves target unchanged and requires the general decoder for
// the complete diagnostics. Collections and non-scalar plans always decline.
func (d *ValueDecoder) TryDecodeParameter(text string, target reflect.Value) bool {
	if d == nil || !d.wire.scalar || !target.IsValid() || !target.CanSet() || target.Type() != d.typ {
		return false
	}
	switch target.Kind() {
	case reflect.String:
		if !utf8.ValidString(text) || !d.alwaysValid && !validScalar(wireValue{kind: 'd', text: text}, d.wire.root) {
			return false
		}
		target.SetString(text)
		return true
	case reflect.Bool:
		var kind byte
		switch text {
		case "true":
			kind = 't'
		case "false":
			kind = 'f'
		default:
			return false
		}
		return decodeScalar(wireValue{kind: kind}, d.wire.root, target, !d.alwaysValid)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return decodeInteger(text, d.wire.root, target, !d.alwaysValid)
	default:
		return false
	}
}

func decodeScalar(value wireValue, n *node, target reflect.Value, checkConstraints bool) bool {
	switch target.Kind() {
	case reflect.String:
		if checkConstraints && !validScalar(value, n) {
			return false
		}
		switch value.kind {
		case 's':
			target.SetString(string(value.raw))
		case 'd':
			target.SetString(value.text)
		default:
			return false
		}
	case reflect.Bool:
		if value.kind != 't' && value.kind != 'f' || checkConstraints && !validScalar(value, n) {
			return false
		}
		target.SetBool(value.kind == 't')
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if value.kind != 'n' {
			return false
		}
		return decodeInteger(string(value.raw), n, target, checkConstraints)
	default:
		return false
	}
	return true
}

func decodeInteger(text string, n *node, target reflect.Value, checkConstraints bool) bool {
	if text == "" {
		return false
	}
	start := 0
	if text[0] == '-' {
		start = 1
	}
	if start == len(text) || text[start] < '0' || text[start] > '9' || text[start] == '0' && start != len(text)-1 {
		return false
	}
	// ParseInt/ParseUint reject remaining non-digits, fractions, exponents,
	// and out-of-range values. This prefix check rejects '+' and leading zeros
	// in both HTTP parameters and native JSON bodies.
	switch target.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		number, err := strconv.ParseInt(text, 10, target.Type().Bits())
		if err != nil {
			return false
		}
		if checkConstraints {
			magnitude := uint64(number)
			if number < 0 {
				magnitude = -magnitude
			}
			if !validNativeInteger(n, numberValue{magnitude: magnitude, negative: number < 0, integer: true}) {
				return false
			}
		}
		target.SetInt(number)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		number, err := strconv.ParseUint(text, 10, target.Type().Bits())
		if err != nil {
			return false
		}
		if checkConstraints && !validNativeInteger(n, numberValue{magnitude: number, integer: true}) {
			return false
		}
		target.SetUint(number)
	default:
		return false
	}
	return true
}

func validScalar(value wireValue, n *node) bool {
	var violations []Violation
	value.validate(n, "", &violations)
	return len(violations) == 0
}

// ParseInt/ParseUint already prove integer notation and the native range. Use
// the same exact constraint checks as typed validation without boxing numeric
// text and parsing it a second time. decodeInteger also checks native notation.
func validNativeInteger(n *node, value numberValue) bool {
	var violations []Violation
	p := valuePlan{node: n}
	p.validateInteger(value, &violations)
	return len(violations) == 0
}
