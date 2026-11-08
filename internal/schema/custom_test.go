package schema_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"trest.local/trest/internal/schema"
)

type declaredText string

func (v declaredText) MarshalText() ([]byte, error)     { return []byte(v), nil }
func (v *declaredText) UnmarshalText(data []byte) error { *v = declaredText(data); return nil }
func (*declaredText) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","minLength":2,"maxLength":12,"pattern":"^[a-z]+$","description":"Lowercase text"}`)
}

type declaredJSON struct{ raw json.RawMessage }

func (v declaredJSON) MarshalJSON() ([]byte, error) {
	if len(v.raw) == 0 {
		return []byte(`null`), nil
	}
	return v.raw, nil
}
func (v *declaredJSON) UnmarshalJSON(data []byte) error { v.raw = bytes.Clone(data); return nil }
func (declaredJSON) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"},"next":{"anyOf":[{"$ref":"#/$defs/node"},{"type":"null"}]}},"required":["label"],"additionalProperties":false,"$defs":{"node":{"type":"object","properties":{"label":{"type":"string"},"next":{"anyOf":[{"$ref":"#/$defs/node"},{"type":"null"}]}},"required":["label"],"additionalProperties":false}}}`)
}

type undeclaredText string

func (v undeclaredText) MarshalText() ([]byte, error)     { return []byte(v), nil }
func (v *undeclaredText) UnmarshalText(data []byte) error { *v = undeclaredText(data); return nil }

type missingDecoder string

func (v missingDecoder) MarshalText() ([]byte, error) { return []byte(v), nil }
func (missingDecoder) TrestSchema() json.RawMessage   { return json.RawMessage(`{"type":"string"}`) }

type pointerEncoder string

func (v *pointerEncoder) MarshalText() ([]byte, error)    { return []byte(*v), nil }
func (v *pointerEncoder) UnmarshalText(data []byte) error { *v = pointerEncoder(data); return nil }
func (pointerEncoder) TrestSchema() json.RawMessage       { return json.RawMessage(`{"type":"string"}`) }

type metadataWithoutCodec string

func (metadataWithoutCodec) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string"}`)
}

type unknownSchemaKeyword struct{ declaredText }

func (unknownSchemaKeyword) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","const":"ok"}`)
}

type mismatchedTextSchema struct{ declaredText }

func (mismatchedTextSchema) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"integer"}`)
}

type nullableTextSchema struct{ declaredText }

func (nullableTextSchema) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":["string","null"]}`)
}

type invalidPatternSchema struct{ declaredText }

func (invalidPatternSchema) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","pattern":"["}`)
}

type invalidLengthSchema struct{ declaredText }

func (invalidLengthSchema) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","minLength":5,"maxLength":2}`)
}

type unsupportedFormatSchema struct{ declaredText }

func (unsupportedFormatSchema) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","format":"unknown-format"}`)
}

type duplicateSchemaKeyword struct{ declaredText }

func (duplicateSchemaKeyword) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","type":"integer"}`)
}

type numericKeywordOnString struct{ declaredText }

func (numericKeywordOnString) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","minimum":1}`)
}

type openObjectSchema struct{ declaredJSON }

func (openObjectSchema) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`)
}

type unknownMapSchemaKeyword struct{ declaredJSON }

func (unknownMapSchemaKeyword) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":{"type":"string","const":"ok"}}`)
}

type panickingSchema struct{ declaredText }

func (panickingSchema) TrestSchema() json.RawMessage { panic("secret metadata") }

type oversizedSchema struct{ declaredText }

func (oversizedSchema) TrestSchema() json.RawMessage {
	return json.RawMessage(strings.Repeat(" ", 65537))
}

type nullSchemaKeyword struct{ declaredText }

func (nullSchemaKeyword) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","minLength":null}`)
}

type invalidDeclaredDefault struct{ declaredText }

func (invalidDeclaredDefault) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","enum":["alpha","beta"],"default":"gamma"}`)
}

type declaredEnumText struct{ declaredText }

func (declaredEnumText) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","enum":["alpha","beta"]}`)
}

type declaredRefText struct{ declaredText }

func (declaredRefText) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"$ref":"#/$defs/text","$defs":{"text":{"type":"string","minLength":2}}}`)
}

type nullableRefText struct{ declaredText }

func (nullableRefText) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"$ref":"#/$defs/text","$defs":{"text":{"anyOf":[{"$ref":"#/$defs/base"},{"type":"null"}]},"base":{"type":"string"}}}`)
}

type declaredNullJSON struct{ declaredJSON }

func (declaredNullJSON) TrestSchema() json.RawMessage { return json.RawMessage(`{"type":"null"}`) }

func TestGenerateDeclaredTextCodec(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[struct {
		Name     declaredText  `json:"name" minLength:"3"`
		Optional *declaredText `json:"optional,omitempty"`
	}]()
	spec, err := schema.Generate(typ)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", `{"name":"hello","optional":"world"}`, true},
		{"nullable pointer", `{"name":"hello","optional":null}`, true},
		{"declared pattern", `{"name":"Hello"}`, false},
		{"field constraint", `{"name":"hi"}`, false},
		{"declared maximum", `{"name":"abcdefghijklmnop"}`, false},
		{"wrong type", `{"name":42}`, false},
		{"unknown field", `{"name":"hello","extra":true}`, false},
		{"duplicate field", `{"name":"hello","name":"world"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			violations, err := schema.Validate(spec, []byte(tc.body))
			if err != nil || (len(violations) == 0) != tc.valid {
				t.Fatalf("violations=%v error=%v", violations, err)
			}
		})
	}
	if !bytes.Contains(spec, []byte(`"description":"Lowercase text"`)) {
		t.Fatalf("declared metadata lost: %s", spec)
	}
}

func TestCustomDefinitionsAreScoped(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[struct {
		First  declaredJSON `json:"first"`
		Second declaredJSON `json:"second"`
		Tree   tree         `json:"tree"`
	}]()
	spec, err := schema.Generate(typ)
	if err != nil {
		t.Fatal(err)
	}
	again, err := schema.Generate(typ)
	if err != nil || !bytes.Equal(spec, again) {
		t.Fatalf("custom definitions are not deterministic: %v", err)
	}
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"independent recursive definitions", `{"first":{"label":"a","next":{"label":"b"}},"second":{"label":"c","next":null},"tree":{"value":"d"}}`, true},
		{"custom closed object", `{"first":{"label":"a","extra":true},"second":{"label":"c"},"tree":{"value":"d"}}`, false},
		{"custom recursive required", `{"first":{"label":"a","next":{}},"second":{"label":"c"},"tree":{"value":"d"}}`, false},
		{"custom duplicate", `{"first":{"label":"a","label":"b"},"second":{"label":"c"},"tree":{"value":"d"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			violations, err := schema.Validate(spec, []byte(tc.body))
			if err != nil || (len(violations) == 0) != tc.valid {
				t.Fatalf("schema=%s violations=%v error=%v", spec, violations, err)
			}
		})
	}
}

func TestRejectsInvalidCustomDeclarations(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		typ  reflect.Type
	}{
		{"missing metadata", reflect.TypeFor[undeclaredText]()},
		{"missing decoder", reflect.TypeFor[missingDecoder]()},
		{"pointer-only encoder", reflect.TypeFor[pointerEncoder]()},
		{"metadata without codec", reflect.TypeFor[metadataWithoutCodec]()},
		{"unknown keyword", reflect.TypeFor[unknownSchemaKeyword]()},
		{"text wire mismatch", reflect.TypeFor[mismatchedTextSchema]()},
		{"nullable text declaration", reflect.TypeFor[nullableTextSchema]()},
		{"invalid pattern", reflect.TypeFor[invalidPatternSchema]()},
		{"reversed bounds", reflect.TypeFor[invalidLengthSchema]()},
		{"unsupported format", reflect.TypeFor[unsupportedFormatSchema]()},
		{"duplicate schema field", reflect.TypeFor[duplicateSchemaKeyword]()},
		{"inapplicable keyword", reflect.TypeFor[numericKeywordOnString]()},
		{"open object", reflect.TypeFor[openObjectSchema]()},
		{"unknown map keyword", reflect.TypeFor[unknownMapSchemaKeyword]()},
		{"metadata panic", reflect.TypeFor[panickingSchema]()},
		{"metadata size", reflect.TypeFor[oversizedSchema]()},
		{"null schema keyword", reflect.TypeFor[nullSchemaKeyword]()},
		{"default outside enum", reflect.TypeFor[invalidDeclaredDefault]()},
		{"nullable referenced text", reflect.TypeFor[nullableRefText]()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := schema.Generate(tc.typ)
			if err == nil {
				t.Fatal("invalid custom declaration accepted")
			}
			if strings.Contains(err.Error(), "secret metadata") {
				t.Fatalf("panic value leaked: %v", err)
			}
		})
	}
}

func TestCustomReferencesAndPointerEnums(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		typ   reflect.Type
		body  string
		valid bool
	}{
		{"pointer enum null", reflect.TypeFor[struct {
			Value *declaredEnumText `json:"value"`
		}](), `{"value":null}`, true},
		{"pointer enum member", reflect.TypeFor[struct {
			Value *declaredEnumText `json:"value"`
		}](), `{"value":"alpha"}`, true},
		{"pointer enum invalid", reflect.TypeFor[struct {
			Value *declaredEnumText `json:"value"`
		}](), `{"value":"gamma"}`, false},
		{"referenced scalar constraint", reflect.TypeFor[struct {
			Value declaredRefText `json:"value" minLength:"3"`
		}](), `{"value":"ab"}`, false},
		{"referenced scalar valid", reflect.TypeFor[struct {
			Value declaredRefText `json:"value" minLength:"3"`
		}](), `{"value":"abc"}`, true},
		{"pointer to null JSON codec", reflect.TypeFor[*declaredNullJSON](), `null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := schema.Generate(tc.typ)
			if err != nil {
				t.Fatal(err)
			}
			violations, err := schema.Validate(spec, []byte(tc.body))
			if err != nil || (len(violations) == 0) != tc.valid {
				t.Fatalf("schema=%s violations=%v error=%v", spec, violations, err)
			}
		})
	}
}

func TestFieldTagsCannotWeakenCustomSchema(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tag  reflect.StructTag
	}{
		{"minimum length", `minLength:"1"`},
		{"maximum length", `maxLength:"20"`},
		{"pattern replacement", `pattern:".*"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typ := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: reflect.TypeFor[declaredText](), Tag: tc.tag}})
			if _, err := schema.Generate(typ); err == nil {
				t.Fatal("field tag weakened custom schema")
			}
		})
	}
}
