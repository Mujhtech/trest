package schema_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"trest.local/trest/internal/schema"
)

func TestCompiledNumericBoundsRemainExact(t *testing.T) {
	t.Parallel()
	const signed = `{"type":"integer","minimum":-9223372036854775808,"maximum":9223372036854775807}`
	const unsigned = `{"type":"integer","minimum":0,"maximum":18446744073709551615}`
	const huge = `{"type":"integer","minimum":-340282366920938463463374607431768211455,"maximum":340282366920938463463374607431768211455}`
	for _, tc := range []struct {
		name, spec, value string
		codes             []string
	}{
		{"signed minimum", signed, "-9223372036854775808", nil},
		{"signed minimum minus one", signed, "-9223372036854775809", []string{"range"}},
		{"signed maximum", signed, "9223372036854775807", nil},
		{"signed maximum plus one", signed, "9223372036854775808", []string{"range"}},
		{"unsigned maximum", unsigned, "18446744073709551615", nil},
		{"unsigned overflow fallback", unsigned, "18446744073709551616", []string{"range"}},
		{"unsigned negative", unsigned, "-1", []string{"range"}},
		{"unsigned negative zero", unsigned, "-0", nil},
		{"huge positive bound", huge, "340282366920938463463374607431768211455", nil},
		{"huge positive overflow", huge, "340282366920938463463374607431768211456", []string{"range"}},
		{"huge negative bound", huge, "-340282366920938463463374607431768211455", nil},
		{"huge negative overflow", huge, "-340282366920938463463374607431768211456", []string{"range"}},
		{"small input huge bound", huge, "1", nil},
		{"fractional lower bound", `{"type":"number","minimum":0.1}`, "0.09999999999999999999999999999999999999", []string{"range"}},
		{"fractional upper bound", `{"type":"number","maximum":0.1}`, "0.10000000000000000000000000000000000001", []string{"range"}},
		{"equivalent fractional bound", `{"type":"number","minimum":0.1,"maximum":0.1}`, "1e-1", nil},
		{"integer input fractional bound", `{"type":"number","minimum":0.5,"maximum":1.5}`, "1", nil},
		{"fractional input integer bound", `{"type":"number","minimum":1,"maximum":2}`, "1.25", nil},
		{"integer decimal notation", signed, "1.0", []string{"type"}},
		{"integer exponent notation", signed, "1e0", []string{"type"}},
		{"finite float overflow", `{"type":"number"}`, "1e309", []string{"range"}},
		{"maximum negative exponent", `{"type":"number"}`, "1e-1000", nil},
		{"negative exponent cap", `{"type":"number"}`, "1e-1001", []string{"range"}},
		{"integer length boundary", `{"type":"integer"}`, strings.Repeat("9", 1024), nil},
		{"integer length cap", `{"type":"integer"}`, strings.Repeat("9", 1025), []string{"range"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validator, err := schema.Compile(json.RawMessage(tc.spec))
			if err != nil {
				t.Fatal(err)
			}
			violations, err := validator.Validate([]byte(tc.value))
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, violation := range violations {
				codes = append(codes, violation.Code)
			}
			if !reflect.DeepEqual(codes, tc.codes) {
				t.Fatalf("codes=%v, want %v", codes, tc.codes)
			}
		})
	}
}

func TestCompiledEnumsPreserveScalarAndNumericEquality(t *testing.T) {
	t.Parallel()
	const integers = `{"type":"integer","enum":[9007199254740993,18446744073709551615,-9223372036854775808]}`
	for _, tc := range []struct {
		name, spec, value string
		codes             []string
	}{
		{"integer above float precision", integers, "9007199254740993", nil},
		{"neighbor below float precision", integers, "9007199254740992", []string{"enum"}},
		{"unsigned maximum", integers, "18446744073709551615", nil},
		{"neighbor below unsigned maximum", integers, "18446744073709551614", []string{"enum"}},
		{"signed minimum", integers, "-9223372036854775808", nil},
		{"integer lexical rule after enum match", `{"type":"integer","enum":[1]}`, "1e0", []string{"type"}},
		{"decimal equals integer member", `{"type":"number","enum":[1]}`, "1.0", nil},
		{"integer equals decimal member", `{"type":"number","enum":[1.0]}`, "1", nil},
		{"rational equality", `{"type":"number","enum":[0.1]}`, "1e-1", nil},
		{"rational inequality despite float rounding", `{"type":"number","enum":[0.1]}`, "0.10000000000000000000000000000000000001", []string{"enum"}},
		{"huge integer equality", `{"type":"integer","enum":[340282366920938463463374607431768211455]}`, "340282366920938463463374607431768211455", nil},
		{"negative zero equality", `{"type":"number","enum":[-0]}`, "0", nil},
		{"overflow error ordering", `{"type":"number","enum":[1]}`, "1e309", []string{"enum", "range"}},
		{"string is not numeric", `{"type":"number","enum":[1]}`, `"1"`, []string{"enum", "type"}},
		{"boolean member", `{"type":"boolean","enum":[true]}`, "true", nil},
		{"boolean rejection", `{"type":"boolean","enum":[true]}`, "false", []string{"enum"}},
		{"nullable enum", `{"type":["integer","null"],"enum":[1,null]}`, "null", nil},
		{"unicode rune length", `{"type":"string","enum":["é🎉"],"minLength":2,"maxLength":2}`, `"é🎉"`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validator, err := schema.Compile(json.RawMessage(tc.spec))
			if err != nil {
				t.Fatal(err)
			}
			violations, err := validator.Validate([]byte(tc.value))
			if err != nil {
				t.Fatal(err)
			}
			var codes []string
			for _, violation := range violations {
				codes = append(codes, violation.Code)
			}
			if !reflect.DeepEqual(codes, tc.codes) {
				t.Fatalf("codes=%v, want %v", codes, tc.codes)
			}
		})
	}
}

func TestCompiledNumericCachesAreSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	validator, err := schema.Compile(json.RawMessage(`{"type":"number","minimum":-9223372036854775808,"maximum":18446744073709551615,"enum":[-9223372036854775808,0.1,1,18446744073709551615]}`))
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 50; j++ {
				for _, value := range []string{"-9223372036854775808", "1e-1", "1.0", "18446744073709551615"} {
					violations, err := validator.Validate([]byte(value))
					if err != nil || len(violations) != 0 {
						t.Errorf("value=%s violations=%v error=%v", value, violations, err)
					}
				}
			}
		}()
	}
	workers.Wait()
}
