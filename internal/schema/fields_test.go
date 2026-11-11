package schema_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/mujhtech/trest/internal/schema"
)

type EmbeddedBase struct {
	ID    string `json:"id"`
	Label string `json:"label" minLength:"2"`
}

type EmbeddedFirst struct {
	Shared string `json:"shared"`
	First  string `json:"first"`
}

type EmbeddedSecond struct {
	Shared int `json:"shared"`
	Second int `json:"second"`
}

type EmbeddedPlain struct{ Code string }
type EmbeddedTagged struct {
	Alias int `json:"Code"`
}

type EmbeddedCycle struct {
	*EmbeddedCycle
	Name string `json:"name"`
}

func TestEmbeddedFieldsFollowJSONEncoding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"value embedding", struct {
			EmbeddedBase
			Enabled bool `json:"enabled"`
		}{EmbeddedBase: EmbeddedBase{ID: "a", Label: "base"}, Enabled: true}},
		{"unexported value embedding", struct{ child }{child: child{Label: "base"}}},
		{"nil pointer embedding", struct{ *EmbeddedBase }{}},
		{"pointer embedding", struct{ *EmbeddedBase }{EmbeddedBase: &EmbeddedBase{ID: "a", Label: "base"}}},
		{"named embedding", struct {
			EmbeddedBase `json:"base"`
		}{EmbeddedBase: EmbeddedBase{ID: "a", Label: "base"}}},
		{"shadowed promoted field", struct {
			EmbeddedBase
			ID int `json:"id"`
		}{EmbeddedBase: EmbeddedBase{ID: "hidden", Label: "base"}, ID: 3}},
		{"ambiguous promoted field", reflect.Zero(ambiguousFieldsType()).Interface()},
		{"tagged promoted name wins", struct {
			EmbeddedPlain
			EmbeddedTagged
		}{EmbeddedPlain: EmbeddedPlain{Code: "hidden"}, EmbeddedTagged: EmbeddedTagged{Alias: 3}}},
		{"recursive embedding", EmbeddedCycle{Name: "root"}},
		{"ignored embedded pointer", struct {
			*child `json:"-"`
			Name   string `json:"name"`
		}{Name: "root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			typ := reflect.TypeOf(tc.value)
			spec, err := schema.Generate(typ)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			violations, err := schema.Validate(spec, encoded)
			if err != nil || len(violations) != 0 {
				t.Fatalf("body=%s, schema=%s, violations=%v, error=%v", encoded, spec, violations, err)
			}
			decoded := reflect.New(typ)
			if err := json.Unmarshal(encoded, decoded.Interface()); err != nil {
				t.Fatalf("schema accepted JSON that cannot initialize Go embedding: %v", err)
			}
		})
	}
}

func TestValidateEmbeddedFields(t *testing.T) {
	t.Parallel()
	type promoted struct{ EmbeddedBase }
	type pointer struct{ *EmbeddedBase }
	for _, tc := range []struct {
		name  string
		typ   reflect.Type
		body  string
		codes []string
	}{
		{"required promoted fields", reflect.TypeFor[promoted](), `{}`, []string{"/id:required", "/label:required"}},
		{"promoted constraints", reflect.TypeFor[promoted](), `{"id":"a","label":"x"}`, []string{"/label:length"}},
		{"closed promoted object", reflect.TypeFor[promoted](), `{"id":"a","label":"ok","unknown":true}`, []string{"/unknown:unknown_field"}},
		{"optional pointer fields", reflect.TypeFor[pointer](), `{}`, nil},
		{"pointer promoted constraints", reflect.TypeFor[pointer](), `{"label":"x"}`, []string{"/label:length"}},
		{"pointer promoted nonnull scalar", reflect.TypeFor[pointer](), `{"label":null}`, []string{"/label:type"}},
		{"ambiguous fields excluded", ambiguousFieldsType(), `{"first":"a","second":2,"shared":3}`, []string{"/shared:unknown_field"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec, err := schema.Generate(tc.typ)
			if err != nil {
				t.Fatal(err)
			}
			violations, err := schema.Validate(spec, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, violation := range violations {
				codes = append(codes, violation.Path+":"+violation.Code)
			}
			if !reflect.DeepEqual(codes, tc.codes) {
				t.Fatalf("violations=%v, want %v", codes, tc.codes)
			}
		})
	}
}

func ambiguousFieldsType() reflect.Type {
	// Build an intentional field conflict dynamically so go vet can continue
	// rejecting accidental duplicate JSON tags in ordinary application types.
	return reflect.StructOf([]reflect.StructField{
		{Name: "EmbeddedFirst", Type: reflect.TypeFor[EmbeddedFirst](), Anonymous: true},
		{Name: "EmbeddedSecond", Type: reflect.TypeFor[EmbeddedSecond](), Anonymous: true},
	})
}

func TestRejectsUndecodableEmbeddedPointer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		typ  reflect.Type
	}{
		{"flattened", reflect.TypeFor[struct{ *child }]()},
		{"named", reflect.TypeFor[struct {
			*child `json:"child"`
		}]()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := schema.Generate(tc.typ); err == nil {
				t.Fatal("unexported embedded pointer cannot be allocated by encoding/json")
			}
		})
	}
}
