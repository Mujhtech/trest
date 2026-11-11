package trestjwt_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mujhtech/trest"
	trestjwt "github.com/mujhtech/trest/adapters/jwt"
)

type claims struct {
	jwt.RegisteredClaims
	Tenant string `json:"tenant"`
}

var now = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func validClaims() *claims {
	return &claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "https://issuer.example", Subject: "user-1", Audience: jwt.ClaimStrings{"trest-api"},
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)),
		},
		Tenant: "acme",
	}
}

func options(key []byte) trestjwt.Options[*claims] {
	return trestjwt.Options[*claims]{
		Algorithm: "HS256", Issuer: "https://issuer.example", Audience: "trest-api",
		NewClaims: func() *claims { return &claims{} },
		Key: func(_ context.Context, kid string) (any, error) {
			if kid != "current" {
				return nil, errors.New("private key lookup details")
			}
			return key, nil
		},
		Principal: func(_ context.Context, c *claims) (trest.Principal, error) {
			return trest.Principal{ID: c.Subject, TenantID: c.Tenant}, nil
		},
		Now: func() time.Time { return now },
	}
}

func signed(t *testing.T, c jwt.Claims, method jwt.SigningMethod, key any, header map[string]any) string {
	t.Helper()
	token := jwt.NewWithClaims(method, c)
	token.Header["kid"] = "current"
	for k, v := range header {
		token.Header[k] = v
	}
	value, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func serve(app http.Handler, values []string, ctx context.Context) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "http://api.example/private", nil)
	if values != nil {
		r.Header["Authorization"] = values
	}
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func newApp(t *testing.T, config trestjwt.Options[*claims], calls *atomic.Int64, protected bool) *trest.App {
	t.Helper()
	authentication, err := trestjwt.New(config)
	if err != nil {
		t.Fatal(err)
	}
	components := []trest.Component{authentication, trest.Get("/private", func(ctx context.Context, _ trest.Empty) (trest.Principal, error) {
		calls.Add(1)
		principal, _ := trest.Identity(ctx)
		return principal, nil
	})}
	if protected {
		components = append(components, trest.RequireAuth())
	}
	app, err := trest.New(components...)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestNewRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*trestjwt.Options[*claims])
	}{
		{"no algorithm", func(o *trestjwt.Options[*claims]) { o.Algorithm = "" }},
		{"none algorithm", func(o *trestjwt.Options[*claims]) { o.Algorithm = "none" }},
		{"custom algorithm", func(o *trestjwt.Options[*claims]) { o.Algorithm = "custom" }},
		{"no issuer", func(o *trestjwt.Options[*claims]) { o.Issuer = " " }},
		{"no audience", func(o *trestjwt.Options[*claims]) { o.Audience = "" }},
		{"no factory", func(o *trestjwt.Options[*claims]) { o.NewClaims = nil }},
		{"no keys", func(o *trestjwt.Options[*claims]) { o.Key = nil }},
		{"no mapper", func(o *trestjwt.Options[*claims]) { o.Principal = nil }},
		{"negative leeway", func(o *trestjwt.Options[*claims]) { o.Leeway = -time.Second }},
		{"unbounded leeway", func(o *trestjwt.Options[*claims]) { o.Leeway = 6 * time.Minute }},
		{"negative size", func(o *trestjwt.Options[*claims]) { o.MaxTokenBytes = -1 }},
		{"unbounded size", func(o *trestjwt.Options[*claims]) { o.MaxTokenBytes = 1 << 20 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := options(make([]byte, 32))
			tc.change(&o)
			if _, err := trestjwt.New(o); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestVerifiedTypedClaimsReachHandler(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("x", 32))
	var calls, mappings atomic.Int64
	o := options(key)
	original := o.Principal
	o.Principal = func(ctx context.Context, c *claims) (trest.Principal, error) {
		mappings.Add(1)
		return original(ctx, c)
	}
	app := newApp(t, o, &calls, true)
	encoded := signed(t, validClaims(), jwt.SigningMethodHS256, key, nil)
	w := serve(app, []string{"bEaReR " + encoded}, nil)
	if w.Code != 200 || calls.Load() != 1 || mappings.Load() != 1 {
		t.Fatalf("status=%d handlers=%d mappings=%d body=%s", w.Code, calls.Load(), mappings.Load(), w.Body)
	}
	var principal trest.Principal
	if err := json.Unmarshal(w.Body.Bytes(), &principal); err != nil {
		t.Fatal(err)
	}
	if principal.ID != "user-1" || principal.TenantID != "acme" {
		t.Fatalf("wrong identity: %+v", principal)
	}
}

func TestInvalidTokensRejectedBeforeMappingAndHandler(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("x", 64))
	for _, tc := range []struct {
		name   string
		change func(*claims)
		method jwt.SigningMethod
		key    any
		header map[string]any
	}{
		{name: "expired", change: func(c *claims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Second)) }},
		{name: "no expiration", change: func(c *claims) { c.ExpiresAt = nil }},
		{name: "wrong issuer", change: func(c *claims) { c.Issuer = "https://evil.example" }},
		{name: "no issuer", change: func(c *claims) { c.Issuer = "" }},
		{name: "wrong audience", change: func(c *claims) { c.Audience = jwt.ClaimStrings{"other-api"} }},
		{name: "no audience", change: func(c *claims) { c.Audience = nil }},
		{name: "not yet valid", change: func(c *claims) { c.NotBefore = jwt.NewNumericDate(now.Add(time.Hour)) }},
		{name: "future issued at", change: func(c *claims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Hour)) }},
		{name: "wrong algorithm", method: jwt.SigningMethodHS512},
		{name: "unsigned", method: jwt.SigningMethodNone, key: jwt.UnsafeAllowNoneSignatureType},
		{name: "invalid signature", key: []byte(strings.Repeat("y", 64))},
		{name: "unknown key", header: map[string]any{"kid": "unknown"}},
		{name: "numeric key", header: map[string]any{"kid": 12}},
		{name: "long key", header: map[string]any{"kid": strings.Repeat("a", 257)}},
		{name: "critical extension", header: map[string]any{"crit": []string{"custom"}}},
		{name: "payload encoding", header: map[string]any{"b64": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validClaims()
			if tc.change != nil {
				tc.change(c)
			}
			method, signingKey := tc.method, tc.key
			if method == nil {
				method = jwt.SigningMethodHS256
			}
			if signingKey == nil {
				signingKey = key
			}
			var calls, mappings atomic.Int64
			o := options(key)
			o.Principal = func(context.Context, *claims) (trest.Principal, error) {
				mappings.Add(1)
				return trest.Principal{ID: "should-not-exist"}, nil
			}
			app := newApp(t, o, &calls, true)
			encoded := signed(t, c, method, signingKey, tc.header)
			w := serve(app, []string{"Bearer " + encoded}, nil)
			assertRejected(t, w)
			if calls.Load() != 0 || mappings.Load() != 0 {
				t.Fatalf("unverified credentials reached handler=%d mapper=%d", calls.Load(), mappings.Load())
			}
		})
	}
}

func assertRejected(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != 401 || !strings.Contains(w.Body.String(), `"message":"invalid bearer token"`) || strings.Contains(w.Body.String(), "private") {
		t.Fatalf("unexpected authentication error: %d %s", w.Code, w.Body)
	}
}

func TestAuthorizationHeaderAndBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		values []string
	}{
		{"empty", []string{""}}, {"duplicate", []string{"Bearer one", "Bearer two"}},
		{"wrong scheme", []string{"Basic abc"}}, {"no token", []string{"Bearer "}},
		{"tab separator", []string{"Bearer\tabc"}}, {"multiple tokens", []string{"Bearer one two"}},
		{"combined headers", []string{"Bearer one, Bearer two"}}, {"malformed token", []string{"Bearer not-a-jwt"}},
		{"too large", []string{"Bearer " + strings.Repeat("a", (16<<10)+1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, keys, mappings atomic.Int64
			o := options(make([]byte, 32))
			o.Key = func(context.Context, string) (any, error) { keys.Add(1); return nil, nil }
			o.Principal = func(context.Context, *claims) (trest.Principal, error) {
				mappings.Add(1)
				return trest.Principal{ID: "wrong"}, nil
			}
			w := serve(newApp(t, o, &calls, false), tc.values, nil)
			assertRejected(t, w)
			if calls.Load()+keys.Load()+mappings.Load() != 0 {
				t.Fatal("invalid header reached application callbacks")
			}
		})
	}
	for _, protected := range []bool{false, true} {
		var calls atomic.Int64
		w := serve(newApp(t, options(make([]byte, 32)), &calls, protected), nil, nil)
		if protected && (w.Code != 401 || calls.Load() != 0) {
			t.Fatalf("anonymous protected route: %d calls=%d", w.Code, calls.Load())
		}
		if !protected && (w.Code != 200 || calls.Load() != 1) {
			t.Fatalf("anonymous public route: %d calls=%d", w.Code, calls.Load())
		}
	}
}

func TestPrincipalAndKeyFailuresArePrivate(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("x", 32))
	encoded := signed(t, validClaims(), jwt.SigningMethodHS256, key, nil)
	for _, tc := range []struct {
		name   string
		change func(*trestjwt.Options[*claims])
	}{
		{"nil claims", func(o *trestjwt.Options[*claims]) { o.NewClaims = func() *claims { return nil } }},
		{"key error", func(o *trestjwt.Options[*claims]) {
			o.Key = func(context.Context, string) (any, error) { return nil, errors.New("private key service failure") }
		}},
		{"short hmac key", func(o *trestjwt.Options[*claims]) {
			o.Key = func(context.Context, string) (any, error) { return []byte("short"), nil }
		}},
		{"mapping error", func(o *trestjwt.Options[*claims]) {
			o.Principal = func(context.Context, *claims) (trest.Principal, error) {
				return trest.Principal{}, trest.NewError(trest.Forbidden, "private account details", nil)
			}
		}},
		{"empty identity", func(o *trestjwt.Options[*claims]) {
			o.Principal = func(context.Context, *claims) (trest.Principal, error) { return trest.Principal{}, nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := options(key)
			tc.change(&o)
			var calls atomic.Int64
			assertRejected(t, serve(newApp(t, o, &calls, true), []string{"Bearer " + encoded}, nil))
			if calls.Load() != 0 {
				t.Fatal("handler ran after authentication failure")
			}
		})
	}
}

func TestCancellationPreserved(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("x", 32))
	encoded := signed(t, validClaims(), jwt.SigningMethodHS256, key, nil)
	for _, tc := range []struct {
		name   string
		stage  string
		err    error
		status int
	}{
		{"canceled before authentication", "before", context.Canceled, 408},
		{"canceled key lookup", "key", context.Canceled, 408},
		{"key deadline", "key", context.DeadlineExceeded, 504},
		{"canceled principal mapping", "principal", context.Canceled, 408},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := options(key)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch tc.stage {
			case "before":
				cancel()
			case "key":
				o.Key = func(context.Context, string) (any, error) { return nil, tc.err }
			case "principal":
				o.Principal = func(context.Context, *claims) (trest.Principal, error) { return trest.Principal{}, tc.err }
			}
			var calls atomic.Int64
			w := serve(newApp(t, o, &calls, true), []string{"Bearer " + encoded}, ctx)
			if w.Code != tc.status || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body)
			}
		})
	}
}

func TestKeyRotationAndConcurrentClaimsIsolation(t *testing.T) {
	t.Parallel()
	oldKey := []byte(strings.Repeat("o", 32))
	newKey := []byte(strings.Repeat("n", 32))
	o := options(nil)
	o.Key = func(_ context.Context, kid string) (any, error) {
		switch kid {
		case "old":
			return oldKey, nil
		case "new":
			return newKey, nil
		default:
			return nil, errors.New("unknown key")
		}
	}
	var calls atomic.Int64
	app := newApp(t, o, &calls, true)
	var wg sync.WaitGroup
	for i := range 20 {
		kid, key := "new", newKey
		if i%2 == 0 {
			kid, key = "old", oldKey
		}
		c := validClaims()
		c.Subject = kid
		encoded := signed(t, c, jwt.SigningMethodHS256, key, map[string]any{"kid": kid})
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := serve(app, []string{"Bearer " + encoded}, nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"`+kid+`"`) {
				t.Errorf("status=%d identity=%s, wanted %s", w.Code, w.Body, kid)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 20 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestEd25519AndLeeway(t *testing.T) {
	t.Parallel()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	o := options(nil)
	o.Algorithm = "EdDSA"
	o.Key = func(context.Context, string) (any, error) { return public, nil }
	o.Leeway = 30 * time.Second
	c := validClaims()
	c.ExpiresAt = jwt.NewNumericDate(now.Add(-20 * time.Second))
	var calls atomic.Int64
	app := newApp(t, o, &calls, true)
	w := serve(app, []string{"Bearer " + signed(t, c, jwt.SigningMethodEdDSA, private, nil)}, nil)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	c.ExpiresAt = jwt.NewNumericDate(now.Add(-31 * time.Second))
	assertRejected(t, serve(app, []string{"Bearer " + signed(t, c, jwt.SigningMethodEdDSA, private, nil)}, nil))
}

func TestNoncanonicalBase64Rejected(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("x", 32))
	encoded := signed(t, validClaims(), jwt.SigningMethodHS256, key, nil)
	parts := strings.Split(encoded, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	// A SHA-256 signature has two unused low bits in its final base64 digit.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := len(parts[2]) - 1
	position := strings.IndexByte(alphabet, parts[2][last])
	parts[2] = parts[2][:last] + string(alphabet[position|1])
	permissive, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || string(permissive) != string(decoded) {
		t.Fatal("invalid test fixture")
	}
	var calls atomic.Int64
	assertRejected(t, serve(newApp(t, options(key), &calls, true), []string{"Bearer " + strings.Join(parts, ".")}, nil))
}

func TestStandardAlgorithms(t *testing.T) {
	t.Parallel()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []*jwt.SigningMethodRSA{jwt.SigningMethodRS256, jwt.SigningMethodRS384, jwt.SigningMethodRS512} {
		t.Run(method.Alg(), func(t *testing.T) { assertAlgorithm(t, method, rsaKey, &rsaKey.PublicKey) })
	}
	for _, method := range []*jwt.SigningMethodRSAPSS{jwt.SigningMethodPS256, jwt.SigningMethodPS384, jwt.SigningMethodPS512} {
		t.Run(method.Alg(), func(t *testing.T) { assertAlgorithm(t, method, rsaKey, &rsaKey.PublicKey) })
	}
	for _, tc := range []struct {
		name   string
		method *jwt.SigningMethodECDSA
		curve  elliptic.Curve
	}{
		{"ES256", jwt.SigningMethodES256, elliptic.P256()},
		{"ES384", jwt.SigningMethodES384, elliptic.P384()},
		{"ES512", jwt.SigningMethodES512, elliptic.P521()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := ecdsa.GenerateKey(tc.curve, rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			assertAlgorithm(t, tc.method, key, &key.PublicKey)
		})
	}
	for _, method := range []*jwt.SigningMethodHMAC{jwt.SigningMethodHS256, jwt.SigningMethodHS384, jwt.SigningMethodHS512} {
		t.Run(method.Alg(), func(t *testing.T) {
			key := []byte(strings.Repeat("x", method.Hash.Size()))
			assertAlgorithm(t, method, key, key)
		})
	}
}

func assertAlgorithm(t *testing.T, method jwt.SigningMethod, signingKey, verificationKey any) {
	t.Helper()
	o := options(nil)
	o.Algorithm = method.Alg()
	o.Key = func(context.Context, string) (any, error) { return verificationKey, nil }
	var calls atomic.Int64
	app := newApp(t, o, &calls, true)
	encoded := signed(t, validClaims(), method, signingKey, nil)
	if w := serve(app, []string{"Bearer " + encoded}, nil); w.Code != 200 {
		t.Fatalf("algorithm %s: %d %s", method.Alg(), w.Code, w.Body)
	}
	// Changing signed payload bytes must fail for every supported verifier.
	parts := strings.Split(encoded, ".")
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"attacker"}`))
	assertRejected(t, serve(app, []string{"Bearer " + strings.Join(parts, ".")}, nil))
	if calls.Load() != 1 {
		t.Fatal("modified token reached handler")
	}
}

func TestAuthorizationHeaderCaseAndDuplicateCasing(t *testing.T) {
	t.Parallel()
	key := []byte(strings.Repeat("x", 32))
	encoded := signed(t, validClaims(), jwt.SigningMethodHS256, key, nil)
	var calls atomic.Int64
	app := newApp(t, options(key), &calls, true)
	r := httptest.NewRequest("GET", "http://api.example/private", nil)
	r.Header["authorization"] = []string{"Bearer " + encoded}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("lowercase header: %d %s", w.Code, w.Body)
	}
	r.Header["Authorization"] = []string{"Bearer " + encoded}
	w = httptest.NewRecorder()
	app.ServeHTTP(w, r)
	assertRejected(t, w)
	if calls.Load() != 1 {
		t.Fatal("duplicate header reached handler")
	}
}
