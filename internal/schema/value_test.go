package schema

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

type fastNamedString string
type plainNamedString string

func (fastNamedString) String() string { return "not the JSON value" }

type valueTextCodec string

func (v valueTextCodec) MarshalText() ([]byte, error)     { return []byte(v), nil }
func (v *valueTextCodec) UnmarshalText(data []byte) error { *v = valueTextCodec(data); return nil }
func (*valueTextCodec) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","minLength":2}`)
}

type valueJSONCodec string

func (v valueJSONCodec) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }
func (v *valueJSONCodec) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*v = valueJSONCodec(text)
	return nil
}
func (valueJSONCodec) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","minLength":2}`)
}

type unconstrainedValueCodec string

func (v unconstrainedValueCodec) MarshalJSON() ([]byte, error) {
	if v == "fail" {
		return nil, errors.New("codec rejected value")
	}
	return json.Marshal(string(v))
}
func (v *unconstrainedValueCodec) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*v = unconstrainedValueCodec(text)
	return nil
}
func (unconstrainedValueCodec) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string"}`)
}

type nativeValueModel struct {
	Text     string `json:"text"`
	Signed   int64  `json:"signed"`
	Unsigned uint64 `json:"unsigned"`
	Flag     bool   `json:"flag"`
	Optional string `json:"optional,omitempty"`
	Nested   struct {
		Text string `json:"text"`
	} `json:"nested,omitempty" required:"true"`
}

func TestValueValidatorStaticTypeProof(t *testing.T) {
	cases := []struct {
		name  string
		value any
		proof bool
	}{
		{"empty struct", struct{}{}, true},
		{"invalid UTF8 string", "\xff\xfe東京", true},
		{"named string", plainNamedString("named"), true},
		{"method-bearing string", fastNamedString("named"), false},
		{"bool", false, true},
		{"minimum signed integer", int64(math.MinInt64), true},
		{"maximum unsigned integer", uint64(math.MaxUint64), true},
		{"native int8 bounds", struct {
			Value int8 `json:"value" min:"-128" max:"127"`
		}{math.MinInt8}, true},
		{"native uint8 bounds", struct {
			Value uint8 `json:"value" min:"0" max:"255"`
		}{math.MaxUint8}, true},
		{"native model", nativeValueModel{}, true},
		{"ignored codec", struct {
			Value unconstrainedValueCodec `json:"-"`
		}{"fail"}, true},
		{"required omitted string", struct {
			Value string `json:"value,omitempty" required:"true"`
		}{}, false},
		{"required omitted bool", struct {
			Value bool `json:"value,omitempty" required:"true"`
		}{}, false},
		{"required omitted integer", struct {
			Value int `json:"value,omitempty" required:"true"`
		}{}, false},
		{"required present string", struct {
			Value string `json:"value,omitempty" required:"true"`
		}{"present"}, false},
		{"optional minimum length", struct {
			Value string `json:"value,omitempty" minLength:"2"`
		}{"x"}, false},
		{"maximum length zero", struct {
			Value string `json:"value" maxLength:"0"`
		}{"x"}, false},
		{"string enum", struct {
			Value string `json:"value" enum:"yes,no"`
		}{"other"}, false},
		{"boolean enum", struct {
			Value bool `json:"value" enum:"true"`
		}{}, false},
		{"pattern", struct {
			Value string `json:"value" pattern:"^yes$"`
		}{"no"}, false},
		{"minimum integer", struct {
			Value int64 `json:"value" min:"-1"`
		}{math.MinInt64}, false},
		{"maximum integer", struct {
			Value uint64 `json:"value" max:"1"`
		}{math.MaxUint64}, false},
		{"float", 1.0, false},
		{"nonfinite float", math.NaN(), false},
		{"pointer", new(int), false},
		{"slice", []int{}, false},
		{"map", map[string]int{}, false},
		{"custom codec", unconstrainedValueCodec("valid"), false},
		{"failing custom codec", unconstrainedValueCodec("fail"), false},
		{"nested custom codec", struct {
			Value unconstrainedValueCodec `json:"value"`
		}{"fail"}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spec, err := Generate(reflect.TypeOf(test.value))
			if err != nil {
				t.Fatal(err)
			}
			wire, err := Compile(spec)
			if err != nil {
				t.Fatal(err)
			}
			typed := wire.ForType(reflect.TypeOf(test.value))
			if typed.AlwaysValid() != test.proof {
				t.Fatalf("static type proof: got %t, want %t", typed.AlwaysValid(), test.proof)
			}
			compareTypedValidation(t, test.value, typed.plan != nil)
			if _, err := typed.Validate(nil); err == nil {
				t.Fatal("static type proof accepted a value of another type")
			}
		})
	}
}

func TestValueValidatorArbitrarySchemaKeepsJSONTagSemantics(t *testing.T) {
	const integerField = `{"type":"object","properties":{"x":{"type":"integer"}},"required":["x"],"additionalProperties":false}`
	cases := []struct {
		name  string
		spec  string
		value any
	}{
		{"string option", integerField, struct {
			Value int `json:"x,string"`
		}{1}},
		{"omitzero option", integerField, struct {
			Value int `json:"x,omitzero"`
		}{}},
		{"unknown option", integerField, struct {
			Value int `json:"x,future"` //nolint:staticcheck // Deliberately verify fallback for an unknown JSON option.
		}{1}},
		{"invalid field name", `{"type":"object","properties":{"x\\y":{"type":"integer"}},"required":["x\\y"],"additionalProperties":false}`, struct {
			Value int `json:"x\\y"` //nolint:staticcheck // Deliberately verify encoding/json's invalid-name fallback.
		}{1}},
		{"invalid symbol name", `{"type":"object","properties":{"🎉":{"type":"integer"}},"required":["🎉"],"additionalProperties":false}`, struct {
			Value int `json:"🎉"`
		}{1}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			wire, err := Compile(json.RawMessage(test.spec))
			if err != nil {
				t.Fatal(err)
			}
			typed := wire.ForType(reflect.TypeOf(test.value))
			if typed.plan != nil || typed.AlwaysValid() {
				t.Fatal("encoding-sensitive JSON tag received a static plan")
			}
			data, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			want, wantErr := wire.Validate(data)
			got, gotErr := typed.Validate(test.value)
			if (wantErr != nil) != (gotErr != nil) || !reflect.DeepEqual(want, got) {
				t.Fatalf("data=%s wire=%+v/%v typed=%+v/%v", data, want, wantErr, got, gotErr)
			}
		})
	}
}

func FuzzValueValidatorStaticTypeProof(f *testing.F) {
	f.Add("hello", int64(1), uint64(2), true, "optional", "nested")
	f.Add("\xff\xfe東京", int64(math.MinInt64), uint64(math.MaxUint64), false, "", "\xed\xa0\x80")
	spec, err := Generate(reflect.TypeFor[nativeValueModel]())
	if err != nil {
		f.Fatal(err)
	}
	wire, err := Compile(spec)
	if err != nil {
		f.Fatal(err)
	}
	typed := wire.ForType(reflect.TypeFor[nativeValueModel]())
	if !typed.AlwaysValid() {
		f.Fatal("native model lacks static type proof")
	}
	f.Fuzz(func(t *testing.T, text string, signed int64, unsigned uint64, flag bool, optional, nested string) {
		value := nativeValueModel{Text: text, Signed: signed, Unsigned: unsigned, Flag: flag, Optional: optional}
		value.Nested.Text = nested
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		issues, err := wire.Validate(data)
		if err != nil || len(issues) != 0 {
			t.Fatalf("static type proof disagrees with encoded contract: %s issues=%+v err=%v", data, issues, err)
		}
		if issues, err := typed.Validate(value); err != nil || len(issues) != 0 {
			t.Fatalf("typed validation: issues=%+v err=%v", issues, err)
		}
		if issues, handled := typed.ValidateNative(reflect.ValueOf(&value).Elem()); !handled || len(issues) != 0 {
			t.Fatalf("native validation: handled=%t issues=%+v", handled, issues)
		}
	})
}

type typedCheckModel struct {
	Text            string `json:"text" minLength:"1" maxLength:"12" pattern:"^[^!]+$"`
	Kind            string `json:"kind" enum:"yes,no"`
	Count           int64  `json:"count" min:"-50" max:"100"`
	Large           uint64 `json:"large"`
	Optional        string `json:"optional,omitempty" minLength:"2"`
	Required        string `json:"required,omitempty" required:"true"`
	DuplicateOption string `json:"duplicate_option,omitempty,omitempty" minLength:"2"` //nolint:staticcheck // Deliberately match encoding/json's repeated-option behavior.
	Hidden          string `json:"-"`
	Inner           struct {
		Flag  bool `json:"flag"`
		Small int8 `json:"small" min:"-1" max:"1"`
	} `json:"inner,omitempty"`
}

func compareTypedValidation(t *testing.T, value any, wantFast bool) {
	t.Helper()
	spec, err := Generate(reflect.TypeOf(value))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	typed := wire.ForType(reflect.TypeOf(value))
	if (typed.plan != nil) != wantFast {
		t.Fatalf("unexpected plan for %T: %v", value, typed.plan != nil)
	}
	data, encodeErr := json.Marshal(value)
	var want []Violation
	var wantErr error
	if encodeErr != nil {
		wantErr = encodeErr
	} else {
		want, wantErr = wire.Validate(data)
	}
	got, gotErr := typed.Validate(value)
	if (wantErr != nil) != (gotErr != nil) || !reflect.DeepEqual(want, got) {
		t.Fatalf("%T %s: wire=%+v (%v) typed=%+v (%v)", value, data, want, wantErr, got, gotErr)
	}
	rv := reflect.ValueOf(value)
	addressable := reflect.New(rv.Type()).Elem()
	addressable.Set(rv)
	for _, input := range []reflect.Value{rv, addressable} {
		issues, handled := typed.ValidateNative(input)
		if handled != wantFast || !handled && issues != nil {
			t.Fatalf("%T: unexpected native handling=%t, issues=%+v", value, handled, issues)
		}
		if handled && (wantErr != nil || !reflect.DeepEqual(want, issues)) {
			t.Fatalf("%T: wire=%+v (%v) native=%+v", value, want, wantErr, issues)
		}
	}
}

func TestValueValidatorNativeDeclinesUnsupportedValues(t *testing.T) {
	spec, err := Generate(reflect.TypeFor[string]())
	if err != nil {
		t.Fatal(err)
	}
	wire, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	typed := wire.ForType(reflect.TypeFor[string]())
	for _, test := range []struct {
		name      string
		validator *ValueValidator
		value     reflect.Value
	}{
		{name: "nil validator", value: reflect.ValueOf("text")},
		{name: "invalid value", validator: typed},
		{name: "different native type", validator: typed, value: reflect.ValueOf(1)},
		{name: "different named string", validator: typed, value: reflect.ValueOf(plainNamedString("text"))},
		{name: "pointer", validator: typed, value: reflect.ValueOf(new(string))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if issues, handled := test.validator.ValidateNative(test.value); handled || issues != nil {
				t.Fatalf("unsupported value handled=%t issues=%+v", handled, issues)
			}
		})
	}
}

func TestValueValidatorNativeConstrainedValueDoesNotAllocate(t *testing.T) {
	type constrained struct {
		Name  string `json:"name" minLength:"1" maxLength:"32"`
		Kind  string `json:"kind" enum:"yes,no"`
		Count int64  `json:"count" min:"0" max:"100"`
	}
	spec, err := Generate(reflect.TypeFor[constrained]())
	if err != nil {
		t.Fatal(err)
	}
	wire, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	typed := wire.ForType(reflect.TypeFor[constrained]())
	if typed.AlwaysValid() {
		t.Fatal("constrained type unexpectedly received an always-valid proof")
	}
	allocations := testing.AllocsPerRun(100, func() {
		value := constrained{Name: "Ada", Kind: "yes", Count: 42}
		issues, handled := typed.ValidateNative(reflect.ValueOf(&value).Elem())
		if !handled || len(issues) != 0 {
			t.Fatalf("native constrained validation handled=%t issues=%+v", handled, issues)
		}
	})
	if allocations != 0 {
		t.Fatalf("native constrained validation allocated %g times", allocations)
	}
}

func TestValueValidatorMatchesEncodedContract(t *testing.T) {
	for _, value := range []any{struct{}{}, "hello", "\xff\xffé", true, int64(math.MinInt64), uint64(math.MaxUint64), typedCheckModel{}, typedCheckModel{Text: "東京", Kind: "yes", Count: 50, Large: math.MaxUint64, Required: "present"}, typedCheckModel{Text: "bad!", Kind: "maybe", Count: 101, Optional: "x", DuplicateOption: "x"}, typedCheckModel{Text: "\xff\xff", Kind: "no", Count: -51}} {
		compareTypedValidation(t, value, true)
	}
}

func TestValueValidatorIntegerBoundariesAndEnums(t *testing.T) {
	type signed struct {
		Value int64 `json:"value" enum:"-9223372036854775808,-1,0,9223372036854775807"`
	}
	type unsigned struct {
		Value uint64 `json:"value" enum:"0,1,18446744073709551615"`
	}
	for _, value := range []any{
		signed{math.MinInt64}, signed{-1}, signed{0}, signed{1}, signed{math.MaxInt64},
		unsigned{0}, unsigned{1}, unsigned{2}, unsigned{math.MaxUint64},
		int8(math.MinInt8), int16(math.MinInt16), int32(math.MinInt32),
		uint8(math.MaxUint8), uint16(math.MaxUint16), uint32(math.MaxUint32),
	} {
		compareTypedValidation(t, value, true)
	}
	// Numeric enum equality accepts equivalent decimal/exponent literals and
	// must retain full uint64 precision even when the bound uses big.Rat.
	wire, err := Compile(json.RawMessage(`{"type":"integer","enum":[-1.0,0e1,1e0,18446744073709551615.0]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{int64(-1), int64(0), int64(1), int64(2), uint64(math.MaxUint64), uint64(math.MaxUint64 - 1)} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		want, wantErr := wire.Validate(data)
		got, gotErr := wire.ForType(reflect.TypeOf(value)).Validate(value)
		if (wantErr != nil) != (gotErr != nil) || !reflect.DeepEqual(want, got) {
			t.Fatalf("%s: wire=%+v (%v) typed=%+v (%v)", data, want, wantErr, got, gotErr)
		}
	}
}

func TestValueValidatorFallsBackForEncodingSensitiveShapes(t *testing.T) {
	type embedded struct{ typedCheckModel }
	type recursive struct {
		Next *recursive `json:"next,omitempty"`
	}
	for _, value := range []any{[]int{1, 2}, map[string]int{"a": 1}, new(int), struct {
		Value float64 `json:"value"`
	}{math.NaN()}, time.Now(), embedded{}, recursive{}} {
		compareTypedValidation(t, value, false)
	}
	for _, value := range []any{valueTextCodec("x"), valueJSONCodec("x"), struct {
		Value valueTextCodec `json:"value"`
	}{"x"}, struct {
		Value valueJSONCodec `json:"value"`
	}{"x"}} {
		compareTypedValidation(t, value, false)
	}
	compareTypedValidation(t, plainNamedString("actual"), true)
	compareTypedValidation(t, fastNamedString("actual"), false)
	compareTypedValidation(t, struct {
		Value int `json:"a/b~c" min:"1"`
	}{0}, true)
}

func FuzzValueValidatorMatchesEncodedContract(f *testing.F) {
	f.Add("hello", "yes", int64(2), uint64(3), "", "")
	f.Add("\xff\xfe東京!", "no", int64(-99), uint64(math.MaxUint64), "x", "present")
	f.Add("\xed\xa0\x80\x80<>&\u2028\u2029", "yes", int64(100), uint64(0), "", "present")
	spec, err := Generate(reflect.TypeFor[typedCheckModel]())
	if err != nil {
		f.Fatal(err)
	}
	wire, err := Compile(spec)
	if err != nil {
		f.Fatal(err)
	}
	typed := wire.ForType(reflect.TypeFor[typedCheckModel]())
	f.Fuzz(func(t *testing.T, text, kind string, count int64, large uint64, optional, required string) {
		value := typedCheckModel{Text: text, Kind: kind, Count: count, Large: large, Optional: optional, Required: required}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		want, wantErr := wire.Validate(data)
		got, gotErr := typed.Validate(value)
		if (wantErr != nil) != (gotErr != nil) || !reflect.DeepEqual(want, got) {
			t.Fatalf("wire=%+v (%v) typed=%+v (%v) data=%s", want, wantErr, got, gotErr, data)
		}
		issues, handled := typed.ValidateNative(reflect.ValueOf(&value).Elem())
		if !handled || wantErr != nil || !reflect.DeepEqual(want, issues) {
			t.Fatalf("wire=%+v (%v) native=%+v handled=%t data=%s", want, wantErr, issues, handled, data)
		}
	})
}
