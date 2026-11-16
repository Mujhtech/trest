package jsonoutput_test

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/mujhtech/trest/internal/jsonoutput"
)

type namedString string
type namedBool bool
type namedInt int64
type namedUint uint64
type numberString json.Number

type valueCodec string

func (valueCodec) MarshalJSON() ([]byte, error) { return []byte(`"custom"`), nil }

type pointerCodec string

func (*pointerCodec) MarshalJSON() ([]byte, error) { return []byte(`"custom"`), nil }

type decodeCodec string

func (*decodeCodec) UnmarshalJSON([]byte) error { return nil }

type valueTextCodec string

func (valueTextCodec) MarshalText() ([]byte, error) { return []byte("custom"), nil }

type pointerTextCodec string

func (*pointerTextCodec) MarshalText() ([]byte, error) { return []byte("custom"), nil }

type decodeTextCodec string

func (*decodeTextCodec) UnmarshalText([]byte) error { return nil }

type appendTextCodec string

func (appendTextCodec) AppendText(dst []byte) ([]byte, error) {
	return append(dst, "custom"...), nil
}

type pointerAppendTextCodec string

func (*pointerAppendTextCodec) AppendText(dst []byte) ([]byte, error) {
	return append(dst, "custom"...), nil
}

type methodString string

func (methodString) String() string { return "method" }

type embedded struct{ Exported string }

func TestNewFallsBackForUnsupportedTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		typ  reflect.Type
	}{
		{"nil", nil},
		{"JSON number", reflect.TypeFor[json.Number]()},
		{"nested JSON number", reflect.TypeFor[struct{ N json.Number }]()},
		{"value JSON codec", reflect.TypeFor[valueCodec]()},
		{"pointer JSON codec", reflect.TypeFor[pointerCodec]()},
		{"JSON decode codec", reflect.TypeFor[decodeCodec]()},
		{"value text codec", reflect.TypeFor[valueTextCodec]()},
		{"pointer text codec", reflect.TypeFor[pointerTextCodec]()},
		{"text decode codec", reflect.TypeFor[decodeTextCodec]()},
		{"text append codec", reflect.TypeFor[appendTextCodec]()},
		{"pointer text append codec", reflect.TypeFor[pointerAppendTextCodec]()},
		{"other exported method", reflect.TypeFor[methodString]()},
		{"nested pointer codec", reflect.TypeFor[struct{ Value pointerCodec }]()},
		{"unexported embedding", reflect.TypeFor[struct{ embedded }]()},
		{"explicitly named embedding", reflect.TypeFor[struct {
			embedded `json:"nested"`
		}]()},
		{"pointer", reflect.TypeFor[*string]()},
		{"map", reflect.TypeFor[map[string]string]()},
		{"slice", reflect.TypeFor[[]string]()},
		{"array", reflect.TypeFor[[1]string]()},
		{"interface", reflect.TypeFor[any]()},
		{"float32", reflect.TypeFor[float32]()},
		{"float64", reflect.TypeFor[float64]()},
		{"complex", reflect.TypeFor[complex128]()},
		{"function", reflect.TypeFor[func()]()},
		{"channel", reflect.TypeFor[chan int]()},
		{"string option", reflect.TypeFor[struct {
			Value int `json:"value,string"`
		}]()},
		{"omitzero option", reflect.TypeFor[struct {
			Value int `json:"value,omitzero"`
		}]()},
		{"unknown option", reflect.TypeFor[struct {
			Value int `json:"value,future"` //nolint:staticcheck // Deliberately exercise unsupported JSON tags.
		}]()},
		{"empty option", reflect.TypeFor[struct {
			Value int `json:"value,"` //nolint:staticcheck // Deliberately exercise unsupported JSON tags.
		}]()},
		{"duplicate JSON names", reflect.StructOf([]reflect.StructField{
			{Name: "First", Type: reflect.TypeFor[string](), Tag: `json:"value"`},
			{Name: "Second", Type: reflect.TypeFor[string](), Tag: `json:"value"`},
		})},
		{"invalid JSON name", reflect.StructOf([]reflect.StructField{
			{Name: "Value", Type: reflect.TypeFor[string](), Tag: `json:"bad\\name"`},
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if plan := jsonoutput.New(test.typ); plan != nil {
				t.Fatalf("New(%v) accepted a type requiring encoding/json fallback", test.typ)
			}
		})
	}
}

func TestAppendMatchesJSON(t *testing.T) {
	t.Parallel()
	type nested struct {
		Text string `json:"text,omitempty"`
		N    int64  `json:"n"`
	}
	tests := []struct {
		name  string
		value any
	}{
		{"empty string", ""},
		{"named string", namedString("hello")},
		{"defined number string has string semantics", numberString("12")},
		{"boolean", true},
		{"named boolean", namedBool(false)},
		{"int", int(-12)},
		{"int8 minimum", int8(math.MinInt8)},
		{"int16 minimum", int16(math.MinInt16)},
		{"int32 minimum", int32(math.MinInt32)},
		{"int64 minimum", int64(math.MinInt64)},
		{"named integer maximum", namedInt(math.MaxInt64)},
		{"uint", uint(12)},
		{"uint8 maximum", uint8(math.MaxUint8)},
		{"uint16 maximum", uint16(math.MaxUint16)},
		{"uint32 maximum", uint32(math.MaxUint32)},
		{"uint64 maximum", uint64(math.MaxUint64)},
		{"named unsigned integer", namedUint(math.MaxUint64)},
		{"uintptr", uintptr(12)},
		{"empty struct", struct{}{}},
		{"declaration order", struct {
			Z string `json:"z"`
			A string `json:"a"`
		}{"last", "first"}},
		{"ignored unsupported fields", struct {
			Keep   string   `json:"keep"`
			Skip   chan int `json:"-"`
			hidden map[string]int
		}{Keep: "visible", hidden: map[string]int{"secret": 1}}},
		{"all optional scalars omitted", struct {
			Text string `json:"text,omitempty"`
			N    int64  `json:"n,omitempty"`
			U    uint64 `json:"u,omitempty"`
			Flag bool   `json:"flag,omitempty"`
		}{}},
		{"optional scalars retained", struct {
			Text namedString `json:"text,omitempty"`
			N    namedInt    `json:"n,omitempty"`
			U    namedUint   `json:"u,omitempty"`
			Flag namedBool   `json:"flag,omitempty"`
		}{"nonempty", -1, 1, true}},
		{"zero nested struct retained", struct {
			Inner nested `json:"inner,omitempty"`
		}{}},
		{"nested struct", struct {
			Inner nested `json:"inner"`
		}{nested{Text: "λ", N: math.MinInt64}}},
		{"default name and empty tag name", struct {
			First  string
			Second string `json:",omitempty"`
		}{"first", "second"}},
		{"HTML and Unicode field names", struct {
			HTML string `json:"<tag>&"`
			Text string `json:"λ"`
		}{"<>&", "中文"}},
		{"dash name with option", struct {
			Value string `json:"-,omitempty"` //nolint:staticcheck // Verify encoding/json's literal dash-name behavior.
		}{"retained"}},
		{"repeated omitempty", struct {
			Value string `json:"value,omitempty,omitempty"` //nolint:staticcheck // Verify repeated options match encoding/json.
		}{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertMatchesJSON(t, test.value)
		})
	}
}

func TestAppendStringBoundaries(t *testing.T) {
	t.Parallel()
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	tests := []struct {
		name string
		text string
	}{
		{"every byte", string(allBytes)},
		{"HTML and quotes", `<script>&"\`},
		{"short escapes", "\b\f\n\r\t"},
		{"ASCII boundary", "\x1f\x20\x7f\x80"},
		{"two byte boundary", "\u0080\u07ff"},
		{"three byte boundary", "\u0800\ud7ff\ue000\uffff"},
		{"four byte boundary", "\U00010000\U0010ffff"},
		{"line separators", "before\u2028between\u2029after"},
		{"replacement rune", "\ufffd"},
		{"isolated continuation", "a\x80\xbfb"},
		{"truncated sequences", "\xc2\xe0\xa0\xf0\x90\x80"},
		{"overlong sequences", "\xc0\xaf\xe0\x80\xaf\xf0\x80\x80\xaf"},
		{"UTF8 surrogate", "\xed\xa0\x80"},
		{"out of range", "\xf4\x90\x80\x80\xf5\x80\x80\x80"},
		{"large mixed string", strings.Repeat("ASCII λ <&> \u2028\xff", 1024)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertMatchesJSON(t, test.text)
		})
	}
}

func TestAppendReadsCurrentValueAndPreservesPrefix(t *testing.T) {
	t.Parallel()
	value := struct {
		Message string `json:"message"`
	}{Message: "first"}
	plan := jsonoutput.New(reflect.TypeOf(value))
	if plan == nil {
		t.Fatal("native struct has no encoding plan")
	}
	first := plan.Append([]byte("prefix:"), reflect.ValueOf(&value).Elem())
	value.Message = "second <&>"
	second := plan.Append(nil, reflect.ValueOf(&value).Elem())
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != `prefix:{"message":"first"}` {
		t.Fatalf("prior output or prefix changed: %q", first)
	}
	if !bytes.Equal(second, want) {
		t.Fatalf("current value was not encoded: got %s, want %s", second, want)
	}
}

func TestAppendNativeValueUsesProvidedStorageWithoutAllocating(t *testing.T) {
	type result struct {
		Message string `json:"message"`
		Count   int64  `json:"count"`
		Enabled bool   `json:"enabled"`
	}
	plan := jsonoutput.New(reflect.TypeFor[result]())
	if plan == nil {
		t.Fatal("native struct has no encoding plan")
	}
	var storage [128]byte
	allocations := testing.AllocsPerRun(100, func() {
		value := result{Message: "hello", Count: 42, Enabled: true}
		encoded := plan.Append(storage[:0], reflect.ValueOf(&value).Elem())
		if string(encoded) != `{"message":"hello","count":42,"enabled":true}` {
			t.Fatalf("unexpected output: %s", encoded)
		}
	})
	if allocations != 0 {
		t.Fatalf("native output allocated %g times with sufficient caller storage", allocations)
	}
}

func FuzzAppendMatchesJSON(f *testing.F) {
	type inner struct {
		Text namedString `json:"text,omitempty"`
		Flag namedBool   `json:"flag,omitempty"`
	}
	type native struct {
		Message  string    `json:"message"`
		Minimum  namedInt  `json:"minimum"`
		Maximum  namedUint `json:"maximum"`
		Optional string    `json:"optional,omitempty"`
		Inner    inner     `json:"inner,omitempty"`
	}
	plan := jsonoutput.New(reflect.TypeFor[native]())
	if plan == nil {
		f.Fatal("native struct has no encoding plan")
	}
	f.Add("hello", int64(0), uint64(0), false)
	f.Add("", int64(math.MinInt64), uint64(math.MaxUint64), true)
	f.Add("\x00\x1f<>&\"\\\u2028\u2029\xff", int64(math.MaxInt64), uint64(1), true)
	f.Add("\xed\xa0\x80\xf4\x90\x80\x80\ufffd", int64(-1), uint64(math.MaxUint64), false)
	f.Fuzz(func(t *testing.T, text string, signed int64, unsigned uint64, flag bool) {
		value := native{Message: text, Minimum: namedInt(signed), Maximum: namedUint(unsigned), Optional: text, Inner: inner{namedString(text), namedBool(flag)}}
		want, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		got := plan.Append(nil, reflect.ValueOf(&value).Elem())
		if !bytes.Equal(got, want) {
			t.Fatalf("JSON differs: got %q, want %q", got, want)
		}
	})
}

func assertMatchesJSON(t *testing.T, value any) {
	t.Helper()
	plan := jsonoutput.New(reflect.TypeOf(value))
	if plan == nil {
		t.Fatalf("New(%T) returned no plan", value)
	}
	want, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	unaddressable := reflect.ValueOf(value)
	addressable := reflect.New(unaddressable.Type()).Elem()
	addressable.Set(unaddressable)
	for _, input := range []reflect.Value{unaddressable, addressable} {
		got := plan.Append(nil, input)
		if !bytes.Equal(got, want) {
			t.Fatalf("JSON differs (addressable=%v): got %q, want %q", input.CanAddr(), got, want)
		}
	}
}
