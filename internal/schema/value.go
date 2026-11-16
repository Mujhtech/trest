package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"
)

// ValueValidator compiles checks for Go values whose JSON representation is
// determined entirely by their static type. More complex models and codecs use
// the full encoded validator. Wire input must still pass Validator.Validate
// before decoding: typed values cannot represent duplicate or unknown fields.
type ValueValidator struct {
	wire *Validator
	typ  reflect.Type
	plan *valuePlan
}

type valuePlan struct {
	node   *node
	path   string
	kind   reflect.Kind
	checks bool
	fields []valueField
}
type valueField struct {
	index     int
	omitEmpty bool
	required  bool
	plan      *valuePlan
}

// ForType binds a compiled schema to its originating Go type. A fast plan is
// used only for closed, nonembedded structs and primitive fields. The remaining
// shapes preserve their encoding/json semantics through the original path.
func (v *Validator) ForType(t reflect.Type) *ValueValidator {
	if v == nil {
		return nil
	}
	return &ValueValidator{wire: v, typ: t, plan: compileValuePlan(t, &v.root, "", 0)}
}

func compileValuePlan(t reflect.Type, n *node, path string, depth int) *valuePlan {
	if t == nil || t == reflect.TypeFor[json.Number]() || depth > 128 || HasCustomCodec(t) || n.reference != nil || len(n.AnyOf) > 0 {
		return nil
	}
	// Later Go toolchains can recognize additional encoding interfaces. Keep
	// method-bearing types on encoding/json's path without depending on those
	// newer interfaces in the Go 1.23 core.
	if t.NumMethod() != 0 || reflect.PointerTo(t).NumMethod() != 0 {
		return nil
	}
	typ, nullable := nodeType(n)
	if nullable {
		return nil
	}
	p := &valuePlan{node: n, path: path, kind: t.Kind()}
	switch t.Kind() {
	case reflect.String:
		if typ != "string" {
			return nil
		}
		p.checks = hasStringChecks(n)
	case reflect.Bool:
		if typ != "boolean" {
			return nil
		}
		p.checks = len(n.Enum) > 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if typ != "integer" {
			return nil
		}
		p.checks = integerTypeNeedsChecks(t, n)
	case reflect.Struct:
		if typ != "object" || n.additional != nil || len(n.Enum) != 0 {
			return nil
		}
		required := make(map[string]bool, len(n.Required))
		for _, name := range n.Required {
			required[name] = true
		}
		seen := make(map[string]bool, len(n.Properties))
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous {
				return nil
			}
			if f.PkgPath != "" || f.Tag.Get("json") == "-" {
				continue
			}
			name, options, hasOptions := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" {
				name = f.Name
			}
			// Generate rejects unsupported tags, but ForType also accepts an
			// independently compiled schema. Do not infer native encoding for
			// options such as string/omitzero, or names encoding/json ignores.
			if !validName(name) {
				return nil
			}
			omitEmpty := false
			if hasOptions {
				for _, option := range strings.Split(options, ",") {
					if option != "omitempty" {
						return nil
					}
					omitEmpty = true
				}
			}
			child, ok := n.Properties[name]
			if !ok || seen[name] {
				return nil
			}
			seen[name] = true
			plan := compileValuePlan(f.Type, child, joinPath(path, name), depth+1)
			if plan == nil {
				return nil
			}
			// Native fields always have the declared JSON type. Keep only
			// checks that can reject a value, including required+omitempty.
			// A struct is never omitted by encoding/json's omitempty rule.
			if plan.checks || omitEmpty && required[name] && f.Type.Kind() != reflect.Struct {
				p.fields = append(p.fields, valueField{index: i, omitEmpty: omitEmpty, required: required[name], plan: plan})
			}
		}
		if len(seen) != len(n.Properties) {
			return nil
		}
		for name := range required {
			if !seen[name] {
				return nil
			}
		}
		p.checks = len(p.fields) > 0
	default:
		return nil
	}
	return p
}

// Generated integer bounds include the native type's own range. Those bounds
// cannot reject a value already stored in that Go type; narrower constraints
// and enums still need runtime checks. Unsigned magnitudes cover MinInt64 and
// MaxUint64 without rounding through floating point or overflowing int64.
func integerTypeNeedsChecks(t reflect.Type, n *node) bool {
	if len(n.Enum) > 0 {
		return true
	}
	minimum := numberValue{integer: true}
	maximum := numberValue{integer: true, magnitude: ^uint64(0) >> (64 - t.Bits())}
	if t.Kind() >= reflect.Int && t.Kind() <= reflect.Int64 {
		minimum.negative = true
		minimum.magnitude = uint64(1) << (t.Bits() - 1)
		maximum.magnitude = minimum.magnitude - 1
	}
	return n.minimumValue != nil && compareNumber(&minimum, n.minimumValue) < 0 ||
		n.maximumValue != nil && compareNumber(&maximum, n.maximumValue) > 0
}

// AlwaysValid reports whether the registered Go type alone proves that every
// value matches this schema. Callers may use it before boxing a value only when
// they already know its exact type, as the generic operation handle does. Wire
// validation and application validation/authorization must still run. Custom
// codecs and encoding-sensitive shapes never receive this proof.
func (v *ValueValidator) AlwaysValid() bool {
	return v != nil && v.plan != nil && !v.plan.checks
}

// Validate checks the exact typed value, including custom codec output when a
// model needs the fallback. It never skips user-defined business validation.
func (v *ValueValidator) Validate(value any) ([]Violation, error) {
	if v == nil {
		return nil, fmt.Errorf("schema: nil value validator")
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() || rv.Type() != v.typ {
		return nil, fmt.Errorf("schema: unexpected Go value type")
	}
	if v.plan == nil {
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return v.wire.Validate(data)
	}
	return v.validateNative(rv), nil
}

// ValidateNative checks an exact typed value without boxing it for the JSON
// fallback. The boolean reports whether a native plan handled the value; false
// requires the caller to use Validate, including its type and codec errors.
// The reflected value is read only and its containing object is not retained.
func (v *ValueValidator) ValidateNative(value reflect.Value) ([]Violation, bool) {
	if v == nil || v.plan == nil || !value.IsValid() || value.Type() != v.typ {
		return nil, false
	}
	return v.validateNative(value), true
}

func (v *ValueValidator) validateNative(value reflect.Value) []Violation {
	if !v.plan.checks {
		return nil
	}
	var violations []Violation
	v.plan.validate(value, &violations)
	if len(violations) > 1 {
		sort.SliceStable(violations, func(i, j int) bool {
			if violations[i].Path == violations[j].Path {
				return violations[i].Code < violations[j].Code
			}
			return violations[i].Path < violations[j].Path
		})
	}
	return violations
}

func (p *valuePlan) validate(value reflect.Value, violations *[]Violation) {
	switch p.kind {
	case reflect.Struct:
		for _, field := range p.fields {
			child := value.Field(field.index)
			// encoding/json's omitempty never omits a struct, including a zero struct.
			if field.omitEmpty && child.Kind() != reflect.Struct && child.IsZero() {
				if field.required {
					*violations = append(*violations, Violation{field.plan.path, "required", "field is required"})
				}
				continue
			}
			field.plan.validate(child, violations)
		}
	case reflect.String:
		text := value.String()
		if !utf8.ValidString(text) {
			// encoding/json replaces each invalid UTF-8 sequence with U+FFFD.
			text = string([]rune(text))
		}
		validateStringValue(p.node, text, p.path, violations)
	case reflect.Bool:
		validateValue(p.node, value.Bool(), p.path, violations)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n := value.Int()
		magnitude := uint64(n)
		if n < 0 {
			// Unsigned negation also handles math.MinInt64 without overflow.
			magnitude = -magnitude
		}
		p.validateInteger(numberValue{magnitude: magnitude, negative: n < 0, integer: true}, violations)
	default:
		p.validateInteger(numberValue{magnitude: value.Uint(), integer: true}, violations)
	}
}

// A Go integer already has valid integer notation and bounded size. Compare its
// exact value directly rather than allocating decimal text and reparsing it.
func (p *valuePlan) validateInteger(value numberValue, violations *[]Violation) {
	if len(p.node.Enum) > 0 {
		matched := false
		for _, member := range p.node.enumValues {
			if member.number != nil && compareNumber(&value, member.number) == 0 {
				matched = true
				break
			}
		}
		if !matched {
			*violations = append(*violations, Violation{p.path, "enum", "value must be an allowed choice"})
		}
	}
	if p.node.minimumValue != nil && compareNumber(&value, p.node.minimumValue) < 0 {
		*violations = append(*violations, Violation{p.path, "range", "number is below the supported minimum"})
	}
	if p.node.maximumValue != nil && compareNumber(&value, p.node.maximumValue) > 0 {
		*violations = append(*violations, Violation{p.path, "range", "number exceeds the supported maximum"})
	}
}
