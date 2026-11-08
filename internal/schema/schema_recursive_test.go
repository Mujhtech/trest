package schema_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"trest.local/trest/internal/schema"
)

type tree struct {
	Value    string           `json:"value" minLength:"1"`
	Next     *tree            `json:"next,omitempty" description:"Next node"`
	Children []tree           `json:"children,omitempty"`
	Lookup   map[string]*tree `json:"lookup,omitempty"`
}

type mutualA struct {
	Name string   `json:"name"`
	B    *mutualB `json:"b,omitempty"`
}

type mutualB struct {
	Count int      `json:"count" min:"1"`
	A     *mutualA `json:"a,omitempty"`
}

type recursiveList []recursiveList
type recursiveMap map[string]recursiveMap

func TestGenerateRecursive(t *testing.T) {
	t.Parallel()
	spec, err := schema.Generate(reflect.TypeFor[tree]())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		again, err := schema.Generate(reflect.TypeFor[tree]())
		if err != nil || string(spec) != string(again) {
			t.Fatalf("nondeterministic recursive schema: %s, error %v", again, err)
		}
	}
	var document struct {
		Type                 string                     `json:"type"`
		Properties           map[string]json.RawMessage `json:"properties"`
		AdditionalProperties bool                       `json:"additionalProperties"`
		Definitions          map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(spec, &document); err != nil {
		t.Fatal(err)
	}
	if document.Type != "object" || document.AdditionalProperties || len(document.Definitions) != 1 {
		t.Fatalf("expected finite, closed inline root and one recursive definition: %s", spec)
	}
	var definition struct {
		Type                 string `json:"type"`
		AdditionalProperties bool   `json:"additionalProperties"`
		Description          string `json:"description"`
	}
	if err := json.Unmarshal(document.Definitions["T0"], &definition); err != nil {
		t.Fatal(err)
	}
	if definition.Type != "object" || definition.AdditionalProperties || definition.Description != "" {
		t.Fatalf("reference target mutated by pointer nullability or field description: %s", spec)
	}
	var next struct {
		AnyOf       []json.RawMessage `json:"anyOf"`
		Description string            `json:"description"`
	}
	if err := json.Unmarshal(document.Properties["next"], &next); err != nil {
		t.Fatal(err)
	}
	if len(next.AnyOf) != 2 || next.Description != "Next node" {
		t.Fatalf("nullable recursive pointer missing union/description: %s", spec)
	}
}

func TestValidateRecursive(t *testing.T) {
	t.Parallel()
	spec, err := schema.Generate(reflect.TypeFor[tree]())
	if err != nil {
		t.Fatal(err)
	}
	validator, err := schema.Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		codes      []string
		wantErr    bool
	}{
		{"leaf", `{"value":"root"}`, nil, false},
		{"null pointer", `{"value":"root","next":null}`, nil, false},
		{"tree", `{"value":"root","next":{"value":"next"},"children":[{"value":"child"}],"lookup":{"key":{"value":"map"},"empty":null}}`, nil, false},
		{"missing child value", `{"value":"root","next":{}}`, []string{"/next/value:required"}, false},
		{"nested constraint", `{"value":"root","next":{"value":""}}`, []string{"/next/value:length"}, false},
		{"unknown child field", `{"value":"root","children":[{"value":"child","secret":true}]}`, []string{"/children/0/secret:unknown_field"}, false},
		{"duplicate map child", `{"value":"root","lookup":{"key":{"value":"a","value":"b"}}}`, []string{"/lookup/key/value:duplicate_field"}, false},
		{"nonnull slice element", `{"value":"root","children":[null]}`, []string{"/children/0:type"}, false},
		{"wrong reference type", `{"value":"root","next":42}`, []string{"/next:type"}, false},
		{"malformed nested", `{"value":"root","next":{"value":}}`, nil, true},
		{"multiple values", `{"value":"root"} {}`, nil, true},
		{"deep recursion", strings.Repeat(`{"value":"x","next":`, 130) + `null` + strings.Repeat("}", 130), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			violations, err := validator.Validate([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			var codes []string
			for _, violation := range violations {
				codes = append(codes, violation.Path+":"+violation.Code)
			}
			if !reflect.DeepEqual(codes, tc.codes) {
				t.Fatalf("violations = %v, want %v", codes, tc.codes)
			}
		})
	}
}

func TestRecursiveForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		body string
	}{
		{"nullable root", reflect.TypeFor[*tree](), `null`},
		{"pointer root", reflect.TypeFor[*tree](), `{"value":"root","next":{"value":"child","next":null}}`},
		{"mutual recursion", reflect.TypeFor[mutualA](), `{"name":"a","b":{"count":1,"a":{"name":"next"}}}`},
		{"recursive slice", reflect.TypeFor[recursiveList](), `[[null,[]],null]`},
		{"recursive map", reflect.TypeFor[recursiveMap](), `{"a":{"b":null},"c":{}}`},
		{"recursive roots in array", reflect.TypeFor[[]tree](), `[{"value":"root","children":[{"value":"child"}]}]`},
		{"recursive roots in map", reflect.TypeFor[map[string]tree](), `{"a":{"value":"root","next":{"value":"child"}}}`},
		{"repeated independent trees", reflect.TypeFor[struct {
			Left  tree `json:"left"`
			Right tree `json:"right"`
		}](), `{"left":{"value":"a"},"right":{"value":"b","next":{"value":"c"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec, err := schema.Generate(tc.typ)
			if err != nil {
				t.Fatal(err)
			}
			violations, err := schema.Validate(spec, []byte(tc.body))
			if err != nil || len(violations) != 0 {
				t.Fatalf("schema=%s, violations=%v, error=%v", spec, violations, err)
			}
		})
	}
}

func TestCompileRejectsInvalidReferences(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, spec string }{
		{"unresolved", `{"$ref":"#/$defs/missing"}`},
		{"external", `{"$ref":"https://example.test/schema"}`},
		{"bad escape", `{"$ref":"#/$defs/a~2b","$defs":{"a~2b":{"type":"string"}}}`},
		{"self alias", `{"$ref":"#/$defs/a","$defs":{"a":{"$ref":"#/$defs/a"}}}`},
		{"mutual aliases", `{"$ref":"#/$defs/a","$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}}}`},
		{"nullable alias cycle", `{"anyOf":[{"$ref":"#/$defs/a"},{"type":"null"}],"$defs":{"a":{"anyOf":[{"$ref":"#/$defs/a"},{"type":"null"}]}}}`},
		{"reference sibling", `{"$ref":"#/$defs/a","type":"string","$defs":{"a":{"type":"string"}}}`},
		{"malformed union", `{"anyOf":[{"$ref":"#/$defs/a"},{"type":"string"}],"$defs":{"a":{"type":"string"}}}`},
		{"constrained null union", `{"anyOf":[{"$ref":"#/$defs/a"},{"type":"null","enum":["x"]}],"$defs":{"a":{"type":"string"}}}`},
		{"nil union branch", `{"anyOf":[null,{"type":"null"}]}`},
		{"invalid nullable type", `{"type":["object","string","null"]}`},
		{"nested definitions", `{"type":"object","properties":{"x":{"type":"string","$defs":{"a":{"type":"string"}}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := schema.Compile(json.RawMessage(tc.spec)); err == nil {
				t.Fatal("invalid schema compiled")
			}
		})
	}
}

func TestRecursivePointerRootKeepsValueChildrenNonnullable(t *testing.T) {
	t.Parallel()
	spec, err := schema.Generate(reflect.TypeFor[*tree]())
	if err != nil {
		t.Fatal(err)
	}
	violations, err := schema.Validate(spec, []byte(`{"value":"root","next":null,"children":[null]}`))
	if err != nil || len(violations) != 1 || violations[0].Path != "/children/0" || violations[0].Code != "type" {
		t.Fatalf("schema=%s, violations=%v, error=%v", spec, violations, err)
	}
}

func TestGenerateRecursiveField(t *testing.T) {
	t.Parallel()
	f := reflect.TypeFor[struct {
		Body tree `description:"A recursive tree"`
	}]().Field(0)
	spec, err := schema.GenerateField(f)
	if err != nil {
		t.Fatal(err)
	}
	violations, err := schema.Validate(spec, []byte(`{"value":"root","next":{"value":"child"}}`))
	if err != nil || len(violations) != 0 || !strings.Contains(string(spec), `"description":"A recursive tree"`) {
		t.Fatalf("schema=%s, violations=%v, error=%v", spec, violations, err)
	}
}

func FuzzValidateRecursive(f *testing.F) {
	spec, err := schema.Generate(reflect.TypeFor[tree]())
	if err != nil {
		f.Fatal(err)
	}
	validator, err := schema.Compile(spec)
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte(`{"value":"root","next":{"value":"leaf"}}`))
	f.Add([]byte(`{"value":"root","next":{"value":null}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64*1024 {
			t.Skip()
		}
		_, _ = validator.Validate(data)
	})
}
