package webinput_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mujhtech/trest"
	"github.com/mujhtech/trest/internal/webinput"
	"github.com/mujhtech/trest/operation"
)

type codecUUID [16]byte

func (v codecUUID) MarshalText() ([]byte, error) {
	s := hex.EncodeToString(v[:])
	return []byte(s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]), nil
}
func (v *codecUUID) UnmarshalText(data []byte) error {
	decoded, err := hex.DecodeString(strings.ReplaceAll(string(data), "-", ""))
	if err != nil || len(decoded) != len(v) {
		return errors.New("private UUID decode failure: " + string(data))
	}
	copy(v[:], decoded)
	return nil
}
func (*codecUUID) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$","minLength":36,"maxLength":36}`)
}

type codecDecimal string

func (v codecDecimal) MarshalJSON() ([]byte, error) { return json.Marshal(string(v)) }
func (v *codecDecimal) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if text == "13.37" {
		return errors.New("private decimal service secret: " + text)
	}
	if text == "66.66" {
		panic("private decimal decoder panic")
	}
	*v = codecDecimal(text)
	return nil
}
func (codecDecimal) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","pattern":"^-?(0|[1-9][0-9]*)\\.[0-9]{2}$","maxLength":24}`)
}

type codecObject struct{ Value string }

func (v codecObject) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"value": v.Value})
}
func (v *codecObject) UnmarshalJSON(data []byte) error {
	var decoded struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	v.Value = decoded.Value
	return nil
}
func (codecObject) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)
}

type codecNamedString string

func (v codecNamedString) MarshalJSON() ([]byte, error) { return json.Marshal("wire:" + string(v)) }
func (v *codecNamedString) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*v = codecNamedString(strings.TrimPrefix(text, "wire:"))
	return nil
}
func (codecNamedString) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"type":"string","pattern":"^wire:"}`)
}
func (codecNamedString) String() string { return "wrong Stringer value" }

type codecRefUUID struct{ codecUUID }

func (codecRefUUID) TrestSchema() json.RawMessage {
	return json.RawMessage(`{"$ref":"#/$defs/alias","$defs":{"alias":{"$ref":"#/$defs/uuid"},"uuid":{"type":"string","pattern":"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"}}}`)
}

func TestDecodeDeclaredCodecsAcrossSources(t *testing.T) {
	t.Parallel()
	const uuid = "12345678-1234-5678-9012-123456789abc"
	type input struct {
		ID      codecUUID     `path:"id"`
		Aliases []codecUUID   `query:"alias"`
		Price   *codecDecimal `query:"price"`
		Header  codecUUID     `header:"X-ID"`
		Cookie  codecUUID     `cookie:"id"`
		Body    struct {
			ID     codecUUID    `json:"id"`
			Amount codecDecimal `json:"amount"`
		} `body:"json"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "POST", "/items/{id}")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/items/"+uuid+"?alias="+uuid+"&alias="+uuid+"&price=12.50", strings.NewReader(`{"id":"`+uuid+`","amount":"9.99"}`))
	r.SetPathValue("id", uuid)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-ID", uuid)
	r.AddCookie(&http.Cookie{Name: "id", Value: uuid})
	value, cleanup, err := plan.Decode(httptest.NewRecorder(), r, 4096)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	got := value.Interface().(input)
	if len(got.Aliases) != 2 || got.ID != got.Header || got.ID != got.Cookie || got.ID != got.Body.ID || got.ID != got.Aliases[0] || got.Price == nil || *got.Price != "12.50" || got.Body.Amount != "9.99" {
		t.Fatalf("custom values were not decoded: %+v", got)
	}
	if !strings.Contains(string(plan.BodySchema), `"pattern"`) || strings.Contains(string(plan.BodySchema), `"format":"byte"`) {
		t.Fatalf("custom wire schema lost: %s", plan.BodySchema)
	}
}

func TestCustomCodecFailuresAreRedacted(t *testing.T) {
	t.Parallel()
	type input struct {
		Amount codecDecimal `query:"amount"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, query, code string }{
		{"wire constraint", "amount=not-a-number", "pattern"},
		{"duplicate scalar", "amount=1.00&amount=2.00", "type"},
		{"decoder error", "amount=13.37", "type"},
		{"decoder panic", "amount=66.66", "type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, cleanup, err := plan.Decode(httptest.NewRecorder(), httptest.NewRequest("GET", "/?"+tc.query, nil), 4096)
			defer cleanup()
			if err == nil || value.IsValid() {
				t.Fatal("invalid custom value returned")
			}
			public := operation.PublicError(err)
			data, marshalErr := json.Marshal(public)
			if marshalErr != nil || public.Code != operation.InvalidInput || len(public.Fields) != 1 || public.Fields[0].Code != tc.code || strings.Contains(string(data), "private") || strings.Contains(string(data), "13.37") || strings.Contains(err.Error(), "private") {
				t.Fatalf("codec error not redacted: %s (%v)", data, err)
			}
		})
	}
}

func TestCustomParameterValidationPrecedesNormalization(t *testing.T) {
	t.Parallel()
	type input struct {
		ID codecUUID `query:"id"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "GET", "/")
	if err != nil {
		t.Fatal(err)
	}
	// The decoder accepts uppercase hex and the encoder emits lowercase. The
	// declared lowercase wire pattern must still reject the original input.
	value, cleanup, err := plan.Decode(httptest.NewRecorder(), httptest.NewRequest("GET", "/?id=12345678-1234-5678-9012-123456789ABC", nil), 4096)
	defer cleanup()
	public := operation.PublicError(err)
	if err == nil || value.IsValid() || len(public.Fields) != 1 || public.Fields[0].Code != "pattern" {
		t.Fatalf("normalization bypassed wire validation: %v", err)
	}
}

func TestCustomObjectParametersAreRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		typ  reflect.Type
	}{
		{"object scalar", reflect.TypeFor[struct {
			Value codecObject `query:"value"`
		}]()},
		{"object collection", reflect.TypeFor[struct {
			Values []codecObject `query:"value"`
		}]()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := webinput.Compile(tc.typ, "GET", "/"); err == nil {
				t.Fatal("object parameter accepted")
			}
		})
	}
}

func TestCustomCodecRouteContractsAndAuthorization(t *testing.T) {
	t.Parallel()
	const uuid = "12345678-1234-5678-9012-123456789abc"
	type input struct {
		ID   codecUUID `path:"id" json:"id"`
		Body struct {
			Amount codecDecimal `json:"amount"`
			Detail codecObject  `json:"detail"`
		} `body:"json" json:"body"`
	}
	type output struct {
		ID     codecUUID    `json:"id"`
		Amount codecDecimal `json:"amount"`
	}
	var calls atomic.Int32
	app, err := trest.New(
		trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
			if r.Header.Get("Authorization") == "allowed" {
				return trest.Principal{ID: "actor"}, nil
			}
			return trest.Principal{}, nil
		}),
		trest.Group("", trest.RequireAuth(), trest.Post("/items/{id}", func(_ context.Context, in input) (output, error) {
			calls.Add(1)
			return output{ID: in.ID, Amount: in.Body.Amount}, nil
		}).Name("customCodec")),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, token string
		status            int
	}{
		{"unauthenticated", `{"amount":"12.50","detail":{"value":"ok"}}`, "", 401},
		{"unknown custom body", `{"amount":"12.50","detail":{"value":"ok","extra":true}}`, "allowed", 400},
		{"duplicate custom body", `{"amount":"12.50","detail":{"value":"ok","value":"other"}}`, "allowed", 400},
		{"private decode error", `{"amount":"13.37","detail":{"value":"ok"}}`, "allowed", 400},
		{"valid", `{"amount":"12.50","detail":{"value":"ok"}}`, "allowed", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/items/"+uuid, strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", tc.token)
			w := httptest.NewRecorder()
			app.ServeHTTP(w, r)
			if w.Code != tc.status || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), "13.37") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if tc.status == 200 && (!strings.Contains(w.Body.String(), uuid) || !strings.Contains(w.Body.String(), `"amount":"12.50"`)) {
				t.Fatalf("custom response changed: %s", w.Body)
			}
		})
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid or unauthorized requests reached handler: %d", calls.Load())
	}
	contract, err := app.OpenAPI()
	if err != nil || !strings.Contains(string(contract), `"pattern"`) {
		t.Fatalf("custom schemas absent: %v", err)
	}
}

func TestCustomOutputViolationIsRedacted(t *testing.T) {
	t.Parallel()
	app, err := trest.New(trest.Get("/", func(context.Context, struct{}) (struct {
		Amount codecDecimal `json:"amount"`
	}, error) {
		return struct {
			Amount codecDecimal `json:"amount"`
		}{Amount: "private invalid amount"}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("invalid custom output leaked: %d %s", w.Code, w.Body)
	}
}

func TestCustomNamedStringOutputUsesItsJSONCodec(t *testing.T) {
	t.Parallel()
	app, err := trest.New(trest.Get("/", func(context.Context, struct{}) (codecNamedString, error) {
		return codecNamedString("hello"), nil
	}).Name("customNamedString"))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || w.Body.String() != `"wire:hello"` || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("custom named-string codec bypassed: %d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	contract, err := app.OpenAPI()
	if err != nil || !strings.Contains(string(contract), `"application/json"`) || strings.Contains(string(contract), `"text/plain"`) {
		t.Fatalf("custom string wire contract incorrect: %s, %v", contract, err)
	}
}

func TestCustomScalarReferencesInFormFields(t *testing.T) {
	t.Parallel()
	const uuid = "12345678-1234-5678-9012-123456789abc"
	type input struct {
		ID codecRefUUID `form:"id"`
	}
	plan, err := webinput.Compile(reflect.TypeFor[input](), "POST", "/")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plan.BodySchema), `"$ref"`) || strings.Contains(string(plan.BodySchema), `"$defs"`) {
		t.Fatalf("scalar reference definitions escaped their form-field scope: %s", plan.BodySchema)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader("id="+uuid))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	value, cleanup, err := plan.Decode(httptest.NewRecorder(), r, 4096)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value.Interface().(input).ID)
	if err != nil || string(encoded) != `"`+uuid+`"` {
		t.Fatalf("form custom codec: %s %v", encoded, err)
	}
}
