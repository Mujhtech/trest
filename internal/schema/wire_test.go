package schema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const flatWireSchema = `{"type":"object","properties":{"name":{"type":"string","minLength":2,"maxLength":8,"enum":["Ada","é🎉","��"]},"count":{"type":"integer","minimum":-9223372036854775808,"maximum":18446744073709551615},"flag":{"type":"boolean"},"optional":{"type":["string","null"]},"a/b~c":{"type":"number","minimum":0.1,"maximum":0.1}},"required":["name","count","flag"],"additionalProperties":false}`

func compareWirePaths(t testing.TB, v *Validator, data []byte) {
	t.Helper()
	got, gotErr := v.Validate(data)
	want, wantErr := v.validateSlow(data)
	if !reflect.DeepEqual(got, want) || (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("data=%q fast=%+v/%v slow=%+v/%v", data, got, gotErr, want, wantErr)
	}
	if gotErr != nil && gotErr.Error() != wantErr.Error() {
		t.Fatalf("errors differ: fast=%v slow=%v", gotErr, wantErr)
	}
}

func TestWireFastPathMatchesFullDecoder(t *testing.T) {
	v, err := Compile(json.RawMessage(flatWireSchema))
	if err != nil {
		t.Fatal(err)
	}
	if v.wire == nil {
		t.Fatal("flat plan missing")
	}
	cases := []struct {
		name string
		data string
		fast bool
	}{
		{"valid", `{"name":"Ada","count":1,"flag":true}`, true},
		{"whitespace reordered nullable unicode", " \t\r\n{ \"flag\" : false, \"name\":\"é🎉\", \"count\":18446744073709551615,\"optional\":null } \r\n", true},
		{"multiple violations sorted escaped path", `{"name":"a","count":18446744073709551616,"flag":"bad","a/b~c":0.100000000000000000001}`, true},
		{"escaped known key minimum integer", `{"n\u0061me":"Ada","count":-9223372036854775808,"flag":true}`, true},
		{"unpaired surrogate integer notation null", `{"name":"\ud800\ud800","count":1e1,"flag":null}`, true},
		{"invalid utf8 value", "{\"name\":\"\xff\xff\",\"count\":1,\"flag\":true}", true},
		{"html and unicode escapes", `{"name":"Ada","count":1,"flag":true,"optional":"<>&\u2028\u2029"}`, true},
		{"escaped quotes and slashes", `{"name":"Ada","count":1,"flag":true,"optional":"a\"b\\c\/d\n\t"}`, true},
		{"duplicate", `{"name":"Ada","name":"x","count":1,"flag":true}`, false},
		{"escaped duplicate", `{"name":"Ada","n\u0061me":"x","count":1,"flag":true}`, false},
		{"missing required", `{"name":"Ada","count":1}`, false},
		{"unknown scalar", `{"name":"Ada","count":1,"flag":true,"unknown":null}`, false},
		{"case sensitive key", `{"Name":"Ada","count":1,"flag":true}`, false},
		{"unknown nested duplicate", `{"name":"Ada","count":1,"flag":true,"unknown":{"x":1,"x":2}}`, false},
		{"known nested duplicate", `{"name":{"x":1,"x":2},"count":1,"flag":true}`, false},
		{"nested array", `{"name":[],"count":1,"flag":true}`, false},
		{"null object", `null`, false}, {"array object", `[]`, false}, {"empty required object", `{}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compareWirePaths(t, v, []byte(tc.data))
			if !json.Valid([]byte(tc.data)) {
				t.Fatalf("invalid fixture %q", tc.data)
			}
			if _, ok := v.wire.validate([]byte(tc.data)); ok != tc.fast {
				t.Fatalf("fast=%t want %t: %s", ok, tc.fast, tc.data)
			}
		})
	}
	for name, data := range map[string]string{
		"empty": "", "incomplete": `{`, "multiple values": `{} {}`, "control character": "{\"name\":\"\x00\"}",
		"invalid escape": `{"name":"\x00"}`, "missing comma": `{"name":"a" "count":1}`,
		"maximum depth":   strings.Repeat("[", 128) + strings.Repeat("]", 128),
		"excessive depth": strings.Repeat("[", 130) + strings.Repeat("]", 130),
	} {
		t.Run(name, func(t *testing.T) { compareWirePaths(t, v, []byte(data)) })
	}
}

var scalarWireSchemas = []struct {
	name string
	spec string
}{
	{"string constraints", `{"type":"string","minLength":2,"maxLength":8,"pattern":"^[^!]+$","enum":["Ada","é🎉","��"]}`},
	{"nullable integer", `{"type":["integer","null"],"minimum":-9223372036854775808,"maximum":18446744073709551615}`},
	{"numeric enum", `{"type":"number","enum":[0.1,1,18446744073709551615]}`},
	{"boolean", `{"type":"boolean"}`},
	{"null", `{"type":"null"}`},
	{"timestamp", `{"type":"string","format":"date-time"}`},
	{"base64", `{"type":"string","contentEncoding":"base64"}`},
}

func TestWireScalarAndUnsupportedPlans(t *testing.T) {
	for _, spec := range scalarWireSchemas {
		t.Run(spec.name, func(t *testing.T) {
			v, err := Compile(json.RawMessage(spec.spec))
			if err != nil {
				t.Fatal(err)
			}
			if v.wire == nil {
				t.Fatal("scalar plan missing")
			}
			for _, data := range []string{
				`null`, `true`, `false`, `-0`, `1`, `1.0`, `0.10000000000000000001`, `18446744073709551615`, `18446744073709551616`,
				`-9223372036854775809`, `1e-1000`, `1e-1001`, `1e99999999999999999999`, `1e309`, strings.Repeat("9", 1024), strings.Repeat("9", 1025),
				`"é🎉"`, `"x!"`, `"Ada"`, `"\ud800"`, `"\ud83c\udf89"`, "\"\xff\"", `"2026-11-10T14:00:00.123456789Z"`, `"2026-13-01"`, `"Zg=="`, `"Zh=="`,
				`[]`, `{"a":1,"a":2}`, `bad`, `"unterminated`, `1 2`,
			} {
				compareWirePaths(t, v, []byte(data))
			}
		})
	}
	for name, spec := range map[string]string{
		"array":              `{"type":"array","items":{"type":"integer"}}`,
		"map":                `{"type":"object","additionalProperties":{"type":"string"}}`,
		"nested object":      `{"type":"object","properties":{"nested":{"type":"object"}}}`,
		"duplicate required": `{"type":"object","properties":{"a":{"type":"string"}},"required":["a","a"]}`,
		"unknown required":   `{"type":"object","properties":{"a":{"type":"string"}},"required":["unknown"]}`,
		"nullable object":    `{"type":["object","null"]}`,
		"object enum":        `{"type":"object","enum":[{}]}`,
		"reference":          `{"$ref":"#/$defs/value","$defs":{"value":{"type":"string"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			v, err := Compile(json.RawMessage(spec))
			if err != nil {
				t.Fatal(err)
			}
			if v.wire != nil {
				t.Fatalf("unsupported shape planned: %s", spec)
			}
			compareWirePaths(t, v, []byte(`{}`))
		})
	}
	for _, count := range []int{64, 65} {
		t.Run(fmt.Sprintf("%d fields", count), func(t *testing.T) {
			properties := map[string]any{}
			body := map[string]int{}
			required := []string{}
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("p%02d", i)
				properties[name] = map[string]string{"type": "integer"}
				body[name] = i
				required = append(required, name)
			}
			spec, err := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required})
			if err != nil {
				t.Fatal(err)
			}
			v, err := Compile(spec)
			if err != nil {
				t.Fatal(err)
			}
			if (v.wire != nil) != (count == 64) {
				t.Fatalf("unexpected plan for %d properties", count)
			}
			data, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			compareWirePaths(t, v, data)
			delete(body, fmt.Sprintf("p%02d", count-1))
			data, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			compareWirePaths(t, v, data)
		})
	}
}

func TestWireDecodedKeysMatchFullDecoder(t *testing.T) {
	v, err := Compile(json.RawMessage(`{"type":"object","properties":{"�":{"type":"string"},"🎉":{"type":"string"},"a\"b\\c":{"type":"string"}},"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"invalid utf8 key":         "{\"\xff\":\"valid\"}",
		"unpaired surrogate key":   `{"\ud800":"valid"}`,
		"surrogate pair key":       `{"\ud83c\udf89":"valid"}`,
		"escaped punctuation key":  `{"a\"b\\c":"valid"}`,
		"normalized duplicate key": "{\"\xff\":\"first\",\"\\ud800\":\"second\"}",
		"optional empty object":    `{}`,
	} {
		t.Run(name, func(t *testing.T) { compareWirePaths(t, v, []byte(data)) })
	}
}

func TestWirePlanConcurrentUse(t *testing.T) {
	v, err := Compile(json.RawMessage(flatWireSchema))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		t.Run(fmt.Sprintf("reader %d", i), func(t *testing.T) {
			t.Parallel()
			for j := 0; j < 30; j++ {
				compareWirePaths(t, v, []byte(`{"name":"Ada","count":1,"flag":true}`))
				compareWirePaths(t, v, []byte(`{"name":"a","count":1.0,"flag":null}`))
			}
		})
	}
}

func FuzzWireValidatorMatchesFullDecoder(f *testing.F) {
	for _, seed := range []string{`{"name":"Ada","count":1,"flag":true}`, `{"name":"a","n\u0061me":"b","count":1,"flag":true}`, `{"name":"\ud800","count":1e-1001,"flag":false}`, "{\"name\":\"\xff\xfe\",\"count\":1,\"flag\":null}", `null`, `{`, `{"unknown":{"a":1,"a":2}}`} {
		f.Add(uint8(0), []byte(seed))
	}
	v, err := Compile(json.RawMessage(flatWireSchema))
	if err != nil {
		f.Fatal(err)
	}
	validators := []*Validator{v}
	for i, schema := range scalarWireSchemas {
		v, err := Compile(json.RawMessage(schema.spec))
		if err != nil {
			f.Fatal(err)
		}
		validators = append(validators, v)
		for _, seed := range []string{`null`, `true`, `1.0`, `18446744073709551615`, `"é🎉"`, `"\ud800"`, `"a\"b\\c"`, "\"\xed\xa0\x80\x80\"", `"2026-11-10T14:00:00Z"`, `"Zh=="`, `[]`} {
			f.Add(uint8(i+1), []byte(seed))
		}
	}
	f.Fuzz(func(t *testing.T, selector uint8, data []byte) {
		compareWirePaths(t, validators[int(selector)%len(validators)], data)
	})
}
