package schema_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"trest.local/trest/internal/schema"
)

type child struct {
	Label string `json:"label"`
}

type payload struct {
	Name     string   `json:"name"`
	Amount   int64    `json:"amount"`
	Enabled  bool     `json:"enabled,omitempty"`
	Optional *child   `json:"optional"`
	Children []child  `json:"children"`
	Pair     [2]int16 `json:"pair"`
}

func TestGenerate(t *testing.T) {
	t.Parallel()
	first, err := schema.Generate(reflect.TypeFor[payload]())
	if err != nil {
		t.Fatal(err)
	}
	second, err := schema.Generate(reflect.TypeFor[payload]())
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("schema generation is not deterministic")
	}
	var got struct {
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Required, []string{"amount", "children", "name", "pair"}) || got.AdditionalProperties {
		t.Fatalf("unexpected object constraints: %s", first)
	}
}

func TestGenerateUnsupported(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		typ  reflect.Type
	}{
		{"nonstring map key", reflect.TypeFor[map[int]int]()},
		{"interface", reflect.TypeFor[any]()},
		{"duplicate names", reflect.StructOf([]reflect.StructField{
			{Name: "First", Type: reflect.TypeFor[string](), Tag: reflect.StructTag(`json:"name"`)},
			{Name: "Other", Type: reflect.TypeFor[string](), Tag: reflect.StructTag(`json:"name"`)},
		})},
		{"string option", reflect.TypeFor[struct {
			Amount int `json:"amount,string"`
		}]()},
		{"omitzero option", reflect.TypeFor[struct {
			Amount int `json:"amount,omitzero"`
		}]()},
		{"channel", reflect.TypeFor[chan string]()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := schema.Generate(tt.typ); err == nil {
				t.Fatalf("expected unsupported type %v", tt.typ)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	spec, err := schema.Generate(reflect.TypeFor[payload]())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		body    string
		codes   []string
		wantErr bool
	}{
		{"valid", `{"name":"invoice","amount":42,"children":[],"pair":[1,2]}`, nil, false},
		{"nullable", `{"name":"invoice","amount":0,"optional":null,"children":null,"pair":[1,2]}`, nil, false},
		{"required", `{"amount":42,"children":[],"pair":[1,2]}`, []string{"/name:required"}, false},
		{"strict case", `{"Name":"invoice","amount":42,"children":[],"pair":[1,2]}`, []string{"/Name:unknown_field", "/name:required"}, false},
		{"unknown", `{"name":"invoice","amount":42,"children":[],"pair":[1,2],"tenant_id":"evil"}`, []string{"/tenant_id:unknown_field"}, false},
		{"duplicate", `{"name":"first","name":"second","amount":42,"children":[],"pair":[1,2]}`, []string{"/name:duplicate_field"}, false},
		{"null scalar", `{"name":null,"amount":42,"children":[],"pair":[1,2]}`, []string{"/name:type"}, false},
		{"nested required", `{"name":"invoice","amount":42,"children":[{}],"pair":[1,2]}`, []string{"/children/0/label:required"}, false},
		{"fixed array", `{"name":"invoice","amount":42,"children":[],"pair":[1]}`, []string{"/pair:length"}, false},
		{"range", `{"name":"invoice","amount":9223372036854775808,"children":[],"pair":[1,2]}`, []string{"/amount:range"}, false},
		{"array range", `{"name":"invoice","amount":42,"children":[],"pair":[40000,2]}`, []string{"/pair/0:range"}, false},
		{"fraction", `{"name":"invoice","amount":1.5,"children":[],"pair":[1,2]}`, []string{"/amount:type"}, false},
		{"exponent integer", `{"name":"invoice","amount":1e2,"children":[],"pair":[1,2]}`, []string{"/amount:type"}, false},
		{"bad json", `{"name":`, nil, true},
		{"trailing value", `{} {}`, nil, true},
		{"trailing data", `{} tail`, nil, true},
		{"empty", ``, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations, err := schema.Validate(spec, []byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, expected error %v", err, tt.wantErr)
			}
			var codes []string
			for _, v := range violations {
				codes = append(codes, v.Path+":"+v.Code)
			}
			if !reflect.DeepEqual(codes, tt.codes) {
				t.Fatalf("violations = %v, want %v", codes, tt.codes)
			}
		})
	}
}

func TestValidateNumericEdges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		typ   reflect.Type
		body  string
		valid bool
	}{
		{"max uint64", reflect.TypeFor[uint64](), "18446744073709551615", true},
		{"overflow uint64", reflect.TypeFor[uint64](), "18446744073709551616", false},
		{"negative uint", reflect.TypeFor[uint64](), "-1", false},
		{"min int64", reflect.TypeFor[int64](), "-9223372036854775808", true},
		{"float", reflect.TypeFor[float64](), "1.25e3", true},
		{"float overflow", reflect.TypeFor[float64](), "1e10000", false},
		{"float extreme underflow", reflect.TypeFor[float64](), "1e-99999999999999999", false},
		{"float32 overflow", reflect.TypeFor[float32](), "3.5e38", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := schema.Generate(tt.typ)
			if err != nil {
				t.Fatal(err)
			}
			violations, err := schema.Validate(spec, []byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if (len(violations) == 0) != tt.valid {
				t.Fatalf("violations: %v", violations)
			}
		})
	}
}

func TestValidateEscapedPathsAndDepth(t *testing.T) {
	t.Parallel()
	spec, err := schema.Generate(reflect.TypeFor[struct {
		Value string `json:"a/b~c"`
	}]())
	if err != nil {
		t.Fatal(err)
	}
	violations, err := schema.Validate(spec, []byte(`{}`))
	if err != nil || len(violations) != 1 || violations[0].Path != "/a~1b~0c" {
		t.Fatalf("violations=%v error=%v", violations, err)
	}
	if _, err := schema.Validate(spec, []byte(strings.Repeat("[", 130)+strings.Repeat("]", 130))); err == nil {
		t.Fatal("deep input accepted")
	}
}

func FuzzValidate(f *testing.F) {
	spec, err := schema.Generate(reflect.TypeFor[payload]())
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte(`{"name":"x","amount":2,"children":[],"pair":[1,2]}`))
	f.Add([]byte(`{"amount":1e99999999999}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64*1024 {
			t.Skip()
		}
		_, _ = schema.Validate(spec, data)
	})
}

func TestStandardTypesAndConstraints(t *testing.T) {
	t.Parallel()
	type request struct {
		Name     string         `minLength:"2" maxLength:"8" pattern:"^[a-z]+$" description:"Display name"`
		Amount   int            `json:"amount" min:"1" max:"100"`
		Currency string         `json:"currency" enum:"NGN,USD" default:"NGN"`
		Tags     []string       `json:"tags" minItems:"1" maxItems:"3"`
		At       time.Time      `json:"at"`
		Labels   map[string]int `json:"labels"`
		Data     []byte         `json:"data"`
	}
	spec, err := schema.Generate(reflect.TypeFor[request]())
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(spec, &document); err != nil {
		t.Fatal(err)
	}
	properties := document["properties"].(map[string]any)
	if properties["Name"].(map[string]any)["minLength"] != float64(2) || properties["currency"].(map[string]any)["default"] != "NGN" {
		t.Fatalf("constraints missing: %s", spec)
	}
	valid := `{"Name":"ada","amount":10,"currency":"NGN","tags":["a"],"at":"2026-11-10T08:00:00Z","labels":{"score":2},"data":"aGk="}`
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", valid, true},
		{"minimum", strings.Replace(valid, `"amount":10`, `"amount":0`, 1), false},
		{"maximum", strings.Replace(valid, `"amount":10`, `"amount":101`, 1), false},
		{"pattern", strings.Replace(valid, `"ada"`, `"Ada"`, 1), false},
		{"length", strings.Replace(valid, `"ada"`, `"a"`, 1), false},
		{"unicode length", strings.Replace(valid, `"ada"`, `"é"`, 1), false},
		{"enum", strings.Replace(valid, `"NGN"`, `"EUR"`, 1), false},
		{"min items", strings.Replace(valid, `["a"]`, `[]`, 1), false},
		{"date", strings.Replace(valid, `2026-11-10T08:00:00Z`, `tomorrow`, 1), false},
		{"map value", strings.Replace(valid, `"score":2`, `"score":"two"`, 1), false},
		{"base64", strings.Replace(valid, `"aGk="`, `"!bad!"`, 1), false},
		{"nullable map bytes", strings.Replace(strings.Replace(valid, `{"score":2}`, `null`, 1), `"aGk="`, `null`, 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			violations, err := schema.Validate(spec, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if (len(violations) == 0) != tc.valid {
				t.Fatalf("violations=%v", violations)
			}
		})
	}
}

func TestRejectsInvalidConstraints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		tag  string
	}{
		{"min on string", reflect.TypeFor[string](), `min:"1"`},
		{"maximum over type", reflect.TypeFor[int8](), `max:"200"`},
		{"reversed range", reflect.TypeFor[int](), `min:"5" max:"2"`},
		{"negative length", reflect.TypeFor[string](), `minLength:"-1"`},
		{"reversed length", reflect.TypeFor[string](), `minLength:"4" maxLength:"2"`},
		{"invalid pattern", reflect.TypeFor[string](), `pattern:"["`},
		{"enum outside range", reflect.TypeFor[int](), `min:"10" enum:"1,2"`},
		{"duplicate enum", reflect.TypeFor[string](), `enum:"a,a"`},
		{"default outside enum", reflect.TypeFor[string](), `enum:"a,b" default:"c"`},
		{"integer fraction", reflect.TypeFor[int](), `min:"1.5"`},
		{"numeric bomb", reflect.TypeFor[float64](), `min:"1e999999999999"`},
		{"invalid required", reflect.TypeFor[string](), `required:"yes"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typ := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: tc.typ, Tag: reflect.StructTag(tc.tag)}})
			if _, err := schema.Generate(typ); err == nil {
				t.Fatal("invalid constraint accepted")
			}
		})
	}
}

func TestCompiledValidatorConcurrent(t *testing.T) {
	t.Parallel()
	spec, err := schema.Generate(reflect.TypeFor[struct {
		Name string `pattern:"^[a-z]+$"`
	}]())
	if err != nil {
		t.Fatal(err)
	}
	validator, err := schema.Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		t.Run("concurrent", func(t *testing.T) {
			t.Parallel()
			for j := 0; j < 20; j++ {
				v, e := validator.Validate([]byte(`{"Name":"ada"}`))
				if e != nil || len(v) > 0 {
					t.Fatalf("%v %v", v, e)
				}
			}
		})
	}
}
