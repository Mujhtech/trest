package webinput_test

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/mujhtech/trest/internal/webinput"
	"github.com/mujhtech/trest/operation"
)

// Method-bearing scalar aliases retain the complete validator/JSON-decoder
// path, giving the native parameter parser an independent behavioral oracle.
func FuzzNativeParametersMatchGeneralPath(f *testing.F) {
	types := [][2]reflect.Type{
		{reflect.TypeFor[struct {
			Value bool `query:"value"`
		}](), reflect.TypeFor[struct {
			Value fallbackParameterBool `query:"value"`
		}]()},
		{reflect.TypeFor[struct {
			Value int8 `query:"value" min:"-100" max:"100"`
		}](), reflect.TypeFor[struct {
			Value fallbackParameterInt `query:"value" min:"-100" max:"100"`
		}]()},
		{reflect.TypeFor[struct {
			Value uint8 `query:"value" min:"1"`
		}](), reflect.TypeFor[struct {
			Value fallbackParameterUint `query:"value" min:"1"`
		}]()},
		{reflect.TypeFor[struct {
			Value string `query:"value" maxLength:"64"`
		}](), reflect.TypeFor[struct {
			Value fallbackParameterString `query:"value" maxLength:"64"`
		}]()},
	}
	plans := make([][2]*webinput.Plan, len(types))
	for i, pair := range types {
		for j, typ := range pair {
			var err error
			plans[i][j], err = webinput.Compile(typ, "GET", "/")
			if err != nil {
				f.Fatal(err)
			}
		}
	}
	for _, seed := range []string{"", "true", "false", "True", "-0", "01", "+1", "100", "101", "255", "256", "1e0", "1.0", " λ😃\n", "\xff\x00"} {
		for i := range plans {
			f.Add(uint8(i), seed, false)
		}
	}
	f.Fuzz(func(t *testing.T, selector uint8, input string, duplicate bool) {
		if len(input) > 4096 {
			t.Skip()
		}
		i := int(selector) % len(plans)
		target := "/?value=" + url.QueryEscape(input)
		if duplicate {
			target += "&value=" + url.QueryEscape(input)
		}
		var values [2]any
		var errs [2]error
		for j, plan := range plans[i] {
			value := reflect.New(types[i][j])
			cleanup, err := plan.DecodeIntoReadOnly(httptest.NewRecorder(), httptest.NewRequest("GET", target, nil), 4096, value.Interface())
			cleanup()
			values[j], errs[j] = value.Elem().Interface(), err
		}
		if (errs[0] == nil) != (errs[1] == nil) {
			t.Fatalf("%d/%q: acceptance changed: native=%v general=%v", i, input, errs[0], errs[1])
		}
		if errs[0] != nil {
			a, b := operation.PublicError(errs[0]), operation.PublicError(errs[1])
			if a.Code != b.Code || a.Message != b.Message || !reflect.DeepEqual(a.Fields, b.Fields) {
				t.Fatalf("%d/%q: diagnostics changed: native=%+v general=%+v", i, input, a, b)
			}
			return
		}
		a, errA := json.Marshal(values[0])
		b, errB := json.Marshal(values[1])
		if errA != nil || errB != nil || string(a) != string(b) {
			t.Fatalf("%d/%q: values changed: native=%s (%v) general=%s (%v)", i, input, a, errA, b, errB)
		}
	})
}
