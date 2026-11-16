package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

type nativeDecodeInput struct {
	Name    string `json:"name" minLength:"1" maxLength:"32"`
	Count   int64  `json:"count" min:"-20" max:"100"`
	Enabled bool   `json:"enabled"`
	Omitted uint16 `json:"omitted,omitempty"`
}

func nativeDecoder(t testing.TB, typ reflect.Type) (*Validator, *ValueDecoder) {
	t.Helper()
	spec, err := Generate(typ)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	decoder := validator.DecoderForType(typ)
	if decoder == nil {
		t.Fatal("native decoder unavailable")
	}
	return validator, decoder
}

func checkNativeDecode(t testing.TB, validator *Validator, decoder *ValueDecoder, data []byte) bool {
	t.Helper()
	var got, want nativeDecodeInput
	ok := decoder.TryDecode(data, reflect.ValueOf(&got).Elem())
	issues, err := validator.validateSlow(data)
	valid := err == nil && len(issues) == 0
	decodeErr := json.Unmarshal(data, &want)
	if ok && (!valid || decodeErr != nil || got != want) {
		t.Fatalf("data=%q: native=%+v; validated=%v issues=%v err=%v std=%+v decodeErr=%v", data, got, valid, issues, err, want, decodeErr)
	}
	return ok
}

func TestNativeDecoderMatchesValidatedJSON(t *testing.T) {
	v, d := nativeDecoder(t, reflect.TypeFor[nativeDecodeInput]())
	for _, input := range []string{
		`{"name":"Ada","count":2,"enabled":true}`,
		` {"enabled":false,"count":-20,"name":"é😃","omitted":65535} `,
		`{"na\u006de":"Ada","count":100,"enabled":true}`,
		`{"name":"\ud800","count":0,"enabled":false}`,
		"{\"name\":\"\xff\",\"count\":0,\"enabled\":false}",
	} {
		if !checkNativeDecode(t, v, d, []byte(input)) {
			t.Fatalf("native input unexpectedly declined: %s", input)
		}
	}
	for _, input := range []string{
		`{"name":"","count":1,"enabled":true}`,
		`{"name":"Ada","count":101,"enabled":true}`,
		`{"name":"Ada","count":-21,"enabled":true}`,
		`{"name":"Ada","count":1.0,"enabled":true}`,
		`{"name":"Ada","count":1e0,"enabled":true}`,
		`{"name":"Ada","count":1,"count":2,"enabled":true}`,
		`{"name":"Ada","count":1,"enabled":true,"extra":null}`,
		`{"name":"Ada","count":1,"enabled":true,"omitted":65536}`,
		`{"name":"Ada","count":1}`,
		`{"name":null,"count":1,"enabled":true}`,
		`{"name":"Ada","count":"1","enabled":true}`,
		`{"name":"Ada","count":1,"enabled":true} {}`,
		`{"name":"Ada","count":1,"enabled":true,}`,
		`{"name":"Ada","count":01,"enabled":true}`,
		`{"name":"Ada","count":-01,"enabled":true}`,
		`{"name":"Ada","count":+1,"enabled":true}`,
		`{"name":"Ada","count":1,"enabled":truX}`,
		`{"name":"Ada","count":1,"enabled":falsX}`,
		`{"name":"Ada","count":1,"enabled":nulX}`,
		`{"name":"Ada","count":1,"enabled":tru}`,
		`{"name":"Ada","count":-,"enabled":true}`,
		`{"name":"a\q","count":1,"enabled":true}`,
		`{"name":"\u12","count":1,"enabled":true}`,
		`{"na\qme":"Ada","count":1,"enabled":true}`,
		"{\"name\":\"a\nb\",\"count\":1,\"enabled\":true}",
		"{\"na\x00me\":\"Ada\",\"count\":1,\"enabled\":true}",
		"{\"name\":\"Ada\",\"count\":1,\"enabled\":true}\x00",
		"{\"name\":\"Ada\",\"count\":1,\"enabled\":true}\ufeff",
		"{\"name\":\"Ada\",\"count\":1,\"enabled\":true}\v",
		`null`, `[]`, `{"name":`,
	} {
		if checkNativeDecode(t, v, d, []byte(input)) {
			t.Fatalf("invalid input accepted: %s", input)
		}
	}
}

func TestNativeDecoderStringsDoNotAliasInput(t *testing.T) {
	_, d := nativeDecoder(t, reflect.TypeFor[nativeDecodeInput]())
	data := []byte(`{"name":"Ada","count":1,"enabled":true}`)
	var value nativeDecodeInput
	if !d.TryDecode(data, reflect.ValueOf(&value).Elem()) {
		t.Fatal("decode failed")
	}
	clear(data)
	if value.Name != "Ada" {
		t.Fatal("decoded string aliases caller bytes")
	}
}

func TestNativeDecoderFieldBitLimit(t *testing.T) {
	fields := make([]reflect.StructField, 65)
	for i := range fields {
		// Reverse wire ordering so a field's wire bit differs from its Go index.
		fields[i] = reflect.StructField{Name: fmt.Sprintf("Field%02d", i), Type: reflect.TypeFor[bool](), Tag: reflect.StructTag(fmt.Sprintf(`json:"f%02d"`, 64-i))}
	}
	typ := reflect.StructOf(fields[:64])
	_, decoder := nativeDecoder(t, typ)
	want := reflect.New(typ).Elem()
	for i := range fields[:64] {
		want.Field(i).SetBool(i%2 == 0)
	}
	data, err := json.Marshal(want.Interface())
	if err != nil {
		t.Fatal(err)
	}
	got := reflect.New(typ).Elem()
	if !decoder.TryDecode(data, got) || !reflect.DeepEqual(got.Interface(), want.Interface()) {
		t.Fatalf("64 fields including the high bit decoded incorrectly: %v", got)
	}
	largeType := reflect.StructOf(fields)
	spec, err := Generate(largeType)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := Compile(spec)
	if err != nil {
		t.Fatal(err)
	}
	if validator.DecoderForType(largeType) != nil {
		t.Fatal("65-field model did not retain the general decoder")
	}
}

func TestNativeIntegerDecoderMatchesWireConstraints(t *testing.T) {
	for _, test := range []struct {
		name, schema string
		typ          reflect.Type
	}{
		{"signed", `{"type":"integer","minimum":-9223372036854775808,"maximum":9223372036854775807}`, reflect.TypeFor[int64]()},
		{"unsigned", `{"type":"integer","minimum":0,"maximum":18446744073709551615}`, reflect.TypeFor[uint64]()},
		{"small", `{"type":"integer","minimum":-4,"maximum":12}`, reflect.TypeFor[int8]()},
		{"native range narrower than schema", `{"type":"integer","minimum":-1000,"maximum":1000}`, reflect.TypeFor[int8]()},
		{"enum", `{"type":"integer","enum":[-2,0,7,9223372036854775807]}`, reflect.TypeFor[int64]()},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator, err := Compile(json.RawMessage(test.schema))
			if err != nil {
				t.Fatal(err)
			}
			decoder := validator.DecoderForType(test.typ)
			if decoder == nil {
				t.Fatal("native integer decoder missing")
			}
			for _, input := range []string{"-9223372036854775809", "-9223372036854775808", "-128", "-5", "-4", "-2", "-1", "-0", "0", "7", "12", "13", "127", "128", "9223372036854775807", "9223372036854775808", "18446744073709551615", "18446744073709551616", "1.0", "1e0", "null", "true", `"7"`, "00"} {
				got, want := reflect.New(test.typ), reflect.New(test.typ)
				accepted := decoder.TryDecode([]byte(input), got.Elem())
				issues, validationErr := validator.validateSlow([]byte(input))
				decodeErr := json.Unmarshal([]byte(input), want.Interface())
				valid := validationErr == nil && len(issues) == 0 && decodeErr == nil
				if accepted != valid || accepted && !reflect.DeepEqual(got.Elem().Interface(), want.Elem().Interface()) {
					t.Fatalf("%s: native=%v value=%v, validated=%v value=%v issues=%v validation=%v decode=%v", input, accepted, got.Elem(), valid, want.Elem(), issues, validationErr, decodeErr)
				}
			}
		})
	}
}

func TestNativeParameterDecoderMatchesValidatedJSON(t *testing.T) {
	for _, test := range []struct {
		name, spec string
		typ        reflect.Type
		valid      []string
		invalid    []string
	}{
		{
			name: "signed", spec: `{"type":"integer","minimum":-9223372036854775808,"maximum":9223372036854775807}`,
			typ:     reflect.TypeFor[int64](),
			valid:   []string{"0", "-0", "1", "-1", "9223372036854775807", "-9223372036854775808"},
			invalid: []string{"", "-", "+1", "00", "01", "-00", "-01", " 1", "1 ", "1\n", "1.0", "1e0", "0x1", "1_0", "true", "null", "9223372036854775808", "-9223372036854775809"},
		},
		{
			name: "unsigned", spec: `{"type":"integer","minimum":0,"maximum":18446744073709551615}`,
			typ:     reflect.TypeFor[uint64](),
			valid:   []string{"0", "1", "18446744073709551615"},
			invalid: []string{"-0", "-1", "+0", "00", "18446744073709551616"},
		},
		{
			name: "integer constraints", spec: `{"type":"integer","minimum":-2,"maximum":7,"enum":[-2,0,7]}`,
			typ: reflect.TypeFor[int8](), valid: []string{"-2", "-0", "0", "7"}, invalid: []string{"-3", "1", "8", "128"},
		},
		{
			name: "boolean", spec: `{"type":"boolean"}`, typ: reflect.TypeFor[bool](),
			valid: []string{"true", "false"}, invalid: []string{"", "True", "FALSE", "1", "0", " true", "false ", "null"},
		},
		{
			name: "boolean enum", spec: `{"type":"boolean","enum":[true]}`, typ: reflect.TypeFor[bool](),
			valid: []string{"true"}, invalid: []string{"false"},
		},
		{
			name: "string", spec: `{"type":"string"}`, typ: reflect.TypeFor[string](),
			valid:   []string{"", "null", "λ😃", "<>&", "\"quoted\"\n\\\t\x00", "\u2028\u2029"},
			invalid: []string{"\xff", "a\xc0\x80b"},
		},
		{
			name: "string constraints", spec: `{"type":"string","minLength":1,"maxLength":3,"pattern":"^[a-zλ]+$","enum":["a","λ"]}`,
			typ: reflect.TypeFor[string](), valid: []string{"a", "λ"}, invalid: []string{"", "abcd", "A", "b"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			validator, err := Compile(json.RawMessage(test.spec))
			if err != nil {
				t.Fatal(err)
			}
			decoder := validator.DecoderForType(test.typ)
			if decoder == nil {
				t.Fatal("native parameter decoder missing")
			}
			for _, input := range test.valid {
				got, want := reflect.New(test.typ), reflect.New(test.typ)
				wire := []byte(input)
				if test.typ.Kind() == reflect.String {
					wire, err = json.Marshal(input)
					if err != nil {
						t.Fatal(err)
					}
				}
				issues, validationErr := validator.validateSlow(wire)
				decodeErr := json.Unmarshal(wire, want.Interface())
				if validationErr != nil || len(issues) != 0 || decodeErr != nil {
					t.Fatalf("oracle rejected %q: issues=%v validation=%v decode=%v", input, issues, validationErr, decodeErr)
				}
				if !decoder.TryDecodeParameter(input, got.Elem()) || !reflect.DeepEqual(got.Elem().Interface(), want.Elem().Interface()) {
					t.Fatalf("%q: native=%v want=%v", input, got.Elem(), want.Elem())
				}
			}
			for _, input := range test.invalid {
				value := reflect.New(test.typ).Elem()
				if decoder.TryDecodeParameter(input, value) || !value.IsZero() {
					t.Fatalf("invalid %q accepted or assigned: %v", input, value)
				}
			}
		})
	}
}

func TestNativeParameterDecoderDeclinesInvalidTargets(t *testing.T) {
	_, decoder := nativeDecoder(t, reflect.TypeFor[string]())
	var nilDecoder *ValueDecoder
	if nilDecoder.TryDecodeParameter("value", reflect.ValueOf(new(string)).Elem()) {
		t.Fatal("nil decoder accepted parameter")
	}
	for _, target := range []reflect.Value{{}, reflect.ValueOf(""), reflect.ValueOf(new(int)).Elem()} {
		if decoder.TryDecodeParameter("value", target) {
			t.Fatal("invalid target accepted parameter")
		}
	}
	_, objectDecoder := nativeDecoder(t, reflect.TypeFor[nativeDecodeInput]())
	if objectDecoder.TryDecodeParameter(`{}`, reflect.ValueOf(new(nativeDecodeInput)).Elem()) {
		t.Fatal("object plan accepted scalar parameter")
	}
}

func TestNativeDecoderRejectsEncodingSensitiveTypes(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[json.Number](),
		reflect.TypeFor[struct{ Number json.Number }](),
		reflect.TypeFor[struct{ Value *string }](),
		reflect.TypeFor[struct{ Value []string }](),
		reflect.TypeFor[struct{ Value float64 }](),
	} {
		spec, err := Generate(typ)
		if err != nil {
			continue
		}
		validator, err := Compile(spec)
		if err != nil {
			t.Fatal(err)
		}
		if decoder := validator.DecoderForType(typ); decoder != nil {
			t.Fatalf("unsafe native decoder for %s", typ)
		}
	}
	v, err := Compile(json.RawMessage(`{"type":"string"}`))
	if err != nil {
		t.Fatal(err)
	}
	if v.ForType(reflect.TypeFor[json.Number]()).AlwaysValid() {
		t.Fatal("json.Number received a native string proof")
	}
	issues, err := v.ForType(reflect.TypeFor[json.Number]()).Validate(json.Number("123"))
	if err != nil || len(issues) == 0 {
		t.Fatalf("json.Number did not retain encoded validation: %v %v", issues, err)
	}
}

func FuzzNativeDecoderMatchesValidatedJSON(f *testing.F) {
	v, d := nativeDecoder(f, reflect.TypeFor[nativeDecodeInput]())
	f.Add([]byte(`{"name":"Ada","count":2,"enabled":true}`))
	f.Add([]byte(`{"name":"\ud800","count":-20,"enabled":false,"omitted":65535}`))
	f.Add([]byte(`{"name":"x","count":9223372036854775808,"enabled":true}`))
	f.Add([]byte(`{"name":"x","count":01,"enabled":truX}`))
	f.Add([]byte("{\"name\":\"a\nb\",\"count\":1,\"enabled\":true}\x00"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		checkNativeDecode(t, v, d, data)
	})
}

func FuzzNativeScalarDecoderMatchesValidatedJSON(f *testing.F) {
	types := []reflect.Type{reflect.TypeFor[int64](), reflect.TypeFor[uint64](), reflect.TypeFor[bool](), reflect.TypeFor[string](), reflect.TypeFor[struct{}]()}
	validators := make([]*Validator, len(types))
	decoders := make([]*ValueDecoder, len(types))
	for i, typ := range types {
		validators[i], decoders[i] = nativeDecoder(f, typ)
	}
	for _, input := range []string{"", "{}", "{}{}", "true", "false", "truX", "falsX", "truefalse", "null", "nulX", "0", "01", "-01", "+1", "-0", "1e0", "-9223372036854775808", "18446744073709551615", `"\ud800"`, `"\u12"`, `"\q"`, "\"\xff\"", "\"a\nb\"", "true\x00", "false\ufeff", "0\v"} {
		for i := range types {
			f.Add(uint8(i), []byte(input))
		}
	}
	f.Fuzz(func(t *testing.T, selector uint8, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		i := int(selector) % len(types)
		got, want := reflect.New(types[i]), reflect.New(types[i])
		accepted := decoders[i].TryDecode(data, got.Elem())
		issues, validationErr := validators[i].validateSlow(data)
		decodeErr := json.Unmarshal(data, want.Interface())
		valid := validationErr == nil && len(issues) == 0 && decodeErr == nil
		if accepted && (!valid || !reflect.DeepEqual(got.Elem().Interface(), want.Elem().Interface())) {
			t.Fatalf("%v/%q: native=%v, validated=%v value=%v issues=%v validation=%v decode=%v", types[i], data, got.Elem(), valid, want.Elem(), issues, validationErr, decodeErr)
		}
	})
}

type nativeUnconstrainedInput struct {
	Text     string `json:"text"`
	Count    int64  `json:"count"`
	Unsigned uint64 `json:"unsigned"`
	Flag     bool   `json:"flag"`
	Optional string `json:"optional,omitempty"`
}

func checkUnconstrainedDecode(t testing.TB, validator *Validator, decoder *ValueDecoder, data []byte) bool {
	t.Helper()
	var got, want nativeUnconstrainedInput
	accepted := decoder.TryDecode(data, reflect.ValueOf(&got).Elem())
	issues, validationErr := validator.validateSlow(data)
	decodeErr := json.Unmarshal(data, &want)
	valid := validationErr == nil && len(issues) == 0 && decodeErr == nil
	if accepted && (!valid || got != want) {
		t.Fatalf("%q: native=%+v validated=%v value=%+v issues=%v validation=%v decode=%v", data, got, valid, want, issues, validationErr, decodeErr)
	}
	if accepted {
		clear(data)
		if got != want {
			t.Fatal("native strings alias the input buffer")
		}
	}
	return accepted
}

func TestNativeDecoderStaticProofPreservesWireChecks(t *testing.T) {
	validator, decoder := nativeDecoder(t, reflect.TypeFor[nativeUnconstrainedInput]())
	if !decoder.alwaysValid {
		t.Fatal("unconstrained native type did not receive a static proof")
	}
	_, constrained := nativeDecoder(t, reflect.TypeFor[nativeDecodeInput]())
	if constrained.alwaysValid {
		t.Fatal("constrained native type received a static proof")
	}
	for _, test := range []struct {
		name, data string
		valid      bool
	}{
		{"zero values", `{"text":"","count":0,"unsigned":0,"flag":false}`, true},
		{"bounds", `{"text":"λ","count":-9223372036854775808,"unsigned":18446744073709551615,"flag":true}`, true},
		{"optional", `{"text":"Ada","count":9223372036854775807,"unsigned":1,"flag":true,"optional":""}`, true},
		{"escaped key and replacement", `{"te\u0078t":"\ud800","count":-0,"unsigned":0,"flag":true}`, true},
		{"invalid UTF8", "{\"text\":\"\xff\",\"count\":1,\"unsigned\":0,\"flag\":false}", true},
		{"missing required", `{"text":"Ada","count":0,"unsigned":0}`, false},
		{"duplicate", `{"text":"Ada","text":"Bob","count":0,"unsigned":0,"flag":false}`, false},
		{"escaped duplicate", `{"text":"Ada","te\u0078t":"Bob","count":0,"unsigned":0,"flag":false}`, false},
		{"unknown", `{"text":"Ada","count":0,"unsigned":0,"flag":false,"extra":1}`, false},
		{"case mismatch", `{"Text":"Ada","count":0,"unsigned":0,"flag":false}`, false},
		{"null string", `{"text":null,"count":0,"unsigned":0,"flag":false}`, false},
		{"number string", `{"text":1,"count":0,"unsigned":0,"flag":false}`, false},
		{"null integer", `{"text":"Ada","count":null,"unsigned":0,"flag":false}`, false},
		{"quoted integer", `{"text":"Ada","count":"0","unsigned":0,"flag":false}`, false},
		{"null boolean", `{"text":"Ada","count":0,"unsigned":0,"flag":null}`, false},
		{"numeric boolean", `{"text":"Ada","count":0,"unsigned":0,"flag":0}`, false},
		{"signed overflow", `{"text":"Ada","count":9223372036854775808,"unsigned":0,"flag":false}`, false},
		{"unsigned overflow", `{"text":"Ada","count":0,"unsigned":18446744073709551616,"flag":false}`, false},
		{"unsigned negative zero", `{"text":"Ada","count":0,"unsigned":-0,"flag":false}`, false},
		{"fraction", `{"text":"Ada","count":1.0,"unsigned":0,"flag":false}`, false},
		{"exponent", `{"text":"Ada","count":1e0,"unsigned":0,"flag":false}`, false},
		{"leading zero", `{"text":"Ada","count":01,"unsigned":0,"flag":false}`, false},
		{"malformed boolean", `{"text":"Ada","count":0,"unsigned":0,"flag":falsX}`, false},
		{"bad escape", `{"text":"\q","count":0,"unsigned":0,"flag":false}`, false},
		{"unescaped control", "{\"text\":\"a\nb\",\"count\":0,\"unsigned\":0,\"flag\":false}", false},
		{"trailing value", `{"text":"Ada","count":0,"unsigned":0,"flag":false} true`, false},
		{"nested string", `{"text":{},"count":0,"unsigned":0,"flag":false}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := checkUnconstrainedDecode(t, validator, decoder, []byte(test.data)); got != test.valid {
				t.Fatalf("accepted=%t, want %t", got, test.valid)
			}
		})
	}
}

func FuzzNativeUnconstrainedDecoderMatchesValidatedJSON(f *testing.F) {
	validator, decoder := nativeDecoder(f, reflect.TypeFor[nativeUnconstrainedInput]())
	for _, input := range []string{
		`{"text":"Ada","count":0,"unsigned":0,"flag":false}`,
		`{"text":"\ud800","count":-9223372036854775808,"unsigned":18446744073709551615,"flag":true}`,
		`{"text":null,"count":0,"unsigned":-0,"flag":null}`,
		`{"text":"Ada","count":01,"unsigned":0,"flag":falsX}`,
		`{"text":"Ada","te\u0078t":"Bob","count":0,"unsigned":0,"flag":false}`,
		"{\"text\":\"\xff\",\"count\":0,\"unsigned\":0,\"flag\":true}",
	} {
		f.Add([]byte(input))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			t.Skip()
		}
		// The ownership assertion clears accepted input, so retain the corpus.
		checkUnconstrainedDecode(t, validator, decoder, append([]byte(nil), data...))
	})
}
