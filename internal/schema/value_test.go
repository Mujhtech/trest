package schema

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"
)

type fastNamedString string

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
	compareTypedValidation(t, fastNamedString("actual"), true)
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
	})
}
