package webinput_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/mujhtech/trest/internal/webinput"
	"github.com/mujhtech/trest/operation"
)

// These types keep encoding/json's native representation but their methods
// deliberately exclude the compiled decoder, providing the general-path oracle.
type fallbackParameterBool bool
type fallbackParameterInt int8
type fallbackParameterUint uint8
type fallbackParameterString string

func (fallbackParameterBool) String() string   { return "unused" }
func (fallbackParameterInt) String() string    { return "unused" }
func (fallbackParameterUint) String() string   { return "unused" }
func (fallbackParameterString) String() string { return "unused" }

type nativeParameterInput struct {
	Enabled  bool    `query:"enabled" default:"false"`
	Count    int8    `query:"count" default:"1" min:"-3" max:"3"`
	Size     uint8   `query:"size" default:"0"`
	Label    string  `query:"label" default:"a" minLength:"1" maxLength:"4" pattern:"^[a-zλ]+$"`
	Optional *int8   `query:"optional" min:"-2" max:"2"`
	Many     []int8  `query:"many" required:"false"`
	Extra    *string `header:"X-Extra"`
}

func TestNativeParameterDecoderMatchesGeneralPath(t *testing.T) {
	t.Parallel()
	type generalInput struct {
		Enabled  fallbackParameterBool   `query:"enabled" default:"false"`
		Count    fallbackParameterInt    `query:"count" default:"1" min:"-3" max:"3"`
		Size     fallbackParameterUint   `query:"size" default:"0"`
		Label    fallbackParameterString `query:"label" default:"a" minLength:"1" maxLength:"4" pattern:"^[a-zλ]+$"`
		Optional *int8                   `query:"optional" min:"-2" max:"2"`
		Many     []int8                  `query:"many" required:"false"`
		Extra    *string                 `header:"X-Extra"`
	}
	nativePlan, err := webinput.Compile(reflect.TypeFor[nativeParameterInput](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	generalPlan, err := webinput.Compile(reflect.TypeFor[generalInput](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, query, path, code string
	}{
		{name: "defaults"},
		{name: "lower and upper boundaries", query: "enabled=true&count=-3&size=255&label=" + url.QueryEscape("λ")},
		{name: "upper signed boundary", query: "count=3&size=1&label=abcd"},
		{name: "negative zero", query: "count=-0"},
		{name: "optional and repeated fields", query: "optional=-2&many=-128&many=127&enabled=true"},
		{name: "signed below min", query: "count=-4", path: "/query/count", code: "range"},
		{name: "signed above max", query: "count=4", path: "/query/count", code: "range"},
		{name: "signed native overflow", query: "count=128", path: "/query/count", code: "range"},
		{name: "unsigned native overflow", query: "size=256", path: "/query/size", code: "range"},
		{name: "unsigned negative", query: "size=-1", path: "/query/size", code: "range"},
		{name: "unsigned negative zero", query: "size=-0", path: "/query/size", code: "type"},
		{name: "noninteger", query: "count=1.0", path: "/query/count", code: "type"},
		{name: "exponent", query: "count=1e0", path: "/query/count", code: "type"},
		{name: "leading zero", query: "count=01", path: "/query/count", code: "type"},
		{name: "negative leading zero", query: "count=-01", path: "/query/count", code: "type"},
		{name: "positive sign", query: "count=%2B1", path: "/query/count", code: "type"},
		{name: "whitespace", query: "count=1%20", path: "/query/count", code: "type"},
		{name: "duplicate integer", query: "count=1&count=2", path: "/query/count", code: "type"},
		{name: "duplicate boolean", query: "enabled=true&enabled=false", path: "/query/enabled", code: "type"},
		{name: "noncanonical boolean", query: "enabled=True", path: "/query/enabled", code: "type"},
		{name: "empty boolean", query: "enabled=", path: "/query/enabled", code: "type"},
		{name: "short string", query: "label=", path: "/query/label", code: "length"},
		{name: "long string", query: "label=abcde", path: "/query/label", code: "length"},
		{name: "string pattern", query: "label=A", path: "/query/label", code: "pattern"},
		{name: "invalid UTF8 string", query: "label=%FF", path: "/query/label", code: "pattern"},
		{name: "optional pointer constraint", query: "optional=3", path: "/query/optional", code: "range"},
		{name: "duplicate pointer", query: "optional=1&optional=2", path: "/query/optional", code: "type"},
		{name: "repeated value constraint", query: "many=1&many=128", path: "/query/many/1", code: "range"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/?"+test.query, nil)
				r.Header.Set("X-Extra", "retained")
				return r
			}
			var got nativeParameterInput
			var want generalInput
			cleanup, gotErr := nativePlan.DecodeIntoReadOnly(httptest.NewRecorder(), request(), 4096, &got)
			defer cleanup()
			cleanup, wantErr := generalPlan.DecodeIntoReadOnly(httptest.NewRecorder(), request(), 4096, &want)
			defer cleanup()
			if test.code == "" {
				if gotErr != nil || wantErr != nil {
					t.Fatalf("native=%v general=%v", gotErr, wantErr)
				}
				gotJSON, gotErr := json.Marshal(got)
				wantJSON, wantErr := json.Marshal(want)
				if gotErr != nil || wantErr != nil || string(gotJSON) != string(wantJSON) {
					t.Fatalf("native=%s (%v) general=%s (%v)", gotJSON, gotErr, wantJSON, wantErr)
				}
				return
			}
			if gotErr == nil || wantErr == nil {
				t.Fatalf("invalid input accepted: native=%v general=%v", gotErr, wantErr)
			}
			gotPublic, wantPublic := operation.PublicError(gotErr), operation.PublicError(wantErr)
			if gotPublic.Code != wantPublic.Code || gotPublic.Message != wantPublic.Message || !reflect.DeepEqual(gotPublic.Fields, wantPublic.Fields) {
				t.Fatalf("diagnostics changed: native=%+v general=%+v", gotPublic, wantPublic)
			}
			if len(gotPublic.Fields) == 0 || gotPublic.Fields[0].Path != test.path || gotPublic.Fields[0].Code != test.code {
				t.Fatalf("unexpected violation: %+v", gotPublic.Fields)
			}
		})
	}
}

func TestNativeStringParameterPreservesJSONNormalization(t *testing.T) {
	t.Parallel()
	type nativeInput struct {
		Value string `query:"value"`
	}
	type generalInput struct {
		Value fallbackParameterString `query:"value"`
	}
	nativePlan, err := webinput.Compile(reflect.TypeFor[nativeInput](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	generalPlan, err := webinput.Compile(reflect.TypeFor[generalInput](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"", "λ😃", "\"quoted\"\n\\\t\x00", "<>&\u2028\u2029", "\xff", "a\xc0\x80b"} {
		request := func() *http.Request {
			return httptest.NewRequest(http.MethodGet, "/?value="+url.QueryEscape(input), nil)
		}
		var got nativeInput
		var want generalInput
		cleanup, gotErr := nativePlan.DecodeIntoReadOnly(httptest.NewRecorder(), request(), 4096, &got)
		cleanup()
		cleanup, wantErr := generalPlan.DecodeIntoReadOnly(httptest.NewRecorder(), request(), 4096, &want)
		cleanup()
		if gotErr != nil || wantErr != nil || got.Value != string(want.Value) {
			t.Fatalf("%q: native=%q (%v), general=%q (%v)", input, got.Value, gotErr, want.Value, wantErr)
		}
	}
}

func TestNativeParameterFailureDoesNotRetainReusedTarget(t *testing.T) {
	t.Parallel()
	plan, err := webinput.Compile(reflect.TypeFor[nativeParameterInput](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	var value nativeParameterInput
	decode := func(query string) error {
		cleanup, err := plan.DecodeIntoReadOnly(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/?"+query, nil), 4096, &value)
		cleanup()
		return err
	}
	if err := decode("count=3&enabled=true&optional=2&many=1&many=2"); err != nil {
		t.Fatal(err)
	}
	if value.Count != 3 || value.Optional == nil || len(value.Many) != 2 {
		t.Fatalf("first request did not populate target: %+v", value)
	}
	if err := decode("count=99"); err == nil {
		t.Fatal("invalid native parameter accepted")
	}
	if value.Count != 0 || value.Optional != nil || value.Many != nil {
		t.Fatalf("failed native fallback retained prior values: %+v", value)
	}
	if err := decode(""); err != nil {
		t.Fatal(err)
	}
	if value.Count != 1 || value.Enabled || value.Label != "a" || value.Optional != nil || value.Many != nil {
		t.Fatalf("later defaults/missing fields changed: %+v", value)
	}
}

type parameterValidationCodec string

func (v parameterValidationCodec) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }

func (v *parameterValidationCodec) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if text == "blocked" {
		panic("wire validation must reject this before the decoder runs")
	}
	if text == "mutate" {
		text = "outside"
	}
	*v = parameterValidationCodec(text)
	return nil
}

func (parameterValidationCodec) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","enum":["valid","mutate"]}`)
}

func TestNativeParameterPlanPreservesCodecValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, query, path string
		typ               reflect.Type
	}{
		{"scalar valid", "value=valid", "", reflect.TypeFor[struct {
			Value parameterValidationCodec `query:"value"`
		}]()},
		{"scalar before decoder", "value=blocked", "/query/value", reflect.TypeFor[struct {
			Value parameterValidationCodec `query:"value"`
		}]()},
		{"scalar after decoder", "value=mutate", "/query/value", reflect.TypeFor[struct {
			Value parameterValidationCodec `query:"value"`
		}]()},
		{"optional missing", "", "", reflect.TypeFor[struct {
			Value *parameterValidationCodec `query:"value"`
		}]()},
		{"optional before decoder", "value=blocked", "/query/value", reflect.TypeFor[struct {
			Value *parameterValidationCodec `query:"value"`
		}]()},
		{"optional after decoder", "value=mutate", "/query/value", reflect.TypeFor[struct {
			Value *parameterValidationCodec `query:"value"`
		}]()},
		{"collection valid", "value=valid&value=valid", "", reflect.TypeFor[struct {
			Value []parameterValidationCodec `query:"value"`
		}]()},
		{"collection before decoder", "value=valid&value=blocked", "/query/value/1", reflect.TypeFor[struct {
			Value []parameterValidationCodec `query:"value"`
		}]()},
		{"collection after decoder", "value=valid&value=mutate", "/query/value/1", reflect.TypeFor[struct {
			Value []parameterValidationCodec `query:"value"`
		}]()},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := webinput.Compile(test.typ, "GET", "/")
			if err != nil {
				t.Fatal(err)
			}
			target := reflect.New(test.typ)
			cleanup, err := plan.DecodeIntoReadOnly(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/?"+test.query, nil), 4096, target.Interface())
			defer cleanup()
			if test.path == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("codec bypassed validation")
			}
			public := operation.PublicError(err)
			if len(public.Fields) != 1 || public.Fields[0].Path != test.path || public.Fields[0].Code != "enum" {
				t.Fatalf("codec ran before wire validation or skipped output validation: %+v", public.Fields)
			}
		})
	}
}

func TestNativeParameterPathsRemainSourceSpecific(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, method, route, target, body, fieldPath string
		typ                                          reflect.Type
	}{
		{"path", "GET", "/items/{id}", "/items/0", "", "/path/id", reflect.TypeFor[struct {
			Value int `path:"id" min:"1"`
		}]()},
		{"escaped query", "GET", "/", "/?x%2Fy~z=0", "", "/query/x~1y~0z", reflect.TypeFor[struct {
			Value int `query:"x/y~z" min:"1"`
		}]()},
		{"canonical header", "GET", "/", "/", "", "/header/X-Count", reflect.TypeFor[struct {
			Value int `header:"x-count" min:"1"`
		}]()},
		{"cookie", "GET", "/", "/", "", "/cookie/count", reflect.TypeFor[struct {
			Value int `cookie:"count" min:"1"`
		}]()},
		{"form", "POST", "/", "/", "count=0", "/form/count", reflect.TypeFor[struct {
			Value int `form:"count" min:"1"`
		}]()},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, err := webinput.Compile(test.typ, test.method, test.route)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(test.method, test.target, strings.NewReader(test.body))
			r.SetPathValue("id", "0")
			r.Header.Set("X-Count", "0")
			r.Header.Set("Cookie", "count=0")
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			_, cleanup, err := plan.Decode(httptest.NewRecorder(), r, 4096)
			defer cleanup()
			if err == nil {
				t.Fatal("invalid parameter accepted")
			}
			public := operation.PublicError(err)
			if len(public.Fields) != 1 || public.Fields[0].Path != test.fieldPath || public.Fields[0].Code != "range" {
				t.Fatalf("source path changed: %+v", public.Fields)
			}
		})
	}
}
