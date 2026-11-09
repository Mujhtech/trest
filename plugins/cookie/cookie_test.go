package cookie

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type claims struct {
	User   string `json:"user"`
	Tenant string `json:"tenant"`
}

func TestSignedCookieRotationTamperAndScope(t *testing.T) {
	old := Key{ID: "old", Secret: bytes.Repeat([]byte{1}, 32)}
	fresh := Key{ID: "new", Secret: bytes.Repeat([]byte{2}, 32)}
	original, err := New[claims](Options{Name: "__Host-session", Keys: []Key{old}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := original.Sign(claims{"alice", "acme"})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := New[claims](Options{Name: "__Host-session", Keys: []Key{fresh, old}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := rotated.Verify(token)
	if err != nil || got.User != "alice" {
		t.Fatalf("%+v %v", got, err)
	}
	newToken, err := rotated.Sign(claims{"alice", "acme"})
	if err != nil || !strings.HasPrefix(newToken, "v1.new.") {
		t.Fatal("new values did not use active key")
	}
	retired, _ := New[claims](Options{Name: "__Host-session", Keys: []Key{fresh}})
	if _, err = retired.Verify(token); !errors.Is(err, ErrInvalid) {
		t.Fatal("retired key accepted")
	}
	wrongPath, _ := New[claims](Options{Name: "__Host-session", Keys: []Key{old}})
	wrongPath.options.Path = "/other"
	if _, err = wrongPath.Verify(token); !errors.Is(err, ErrInvalid) {
		t.Fatal("cookie path substitution accepted")
	}
	wrongName, _ := New[claims](Options{Name: "other-session", Keys: []Key{old}})
	if _, err = wrongName.Verify(token); !errors.Is(err, ErrInvalid) {
		t.Fatal("cookie scope substitution accepted")
	}
	parts := strings.Split(token, ".")
	parts[3] = "e30"
	if _, err = rotated.Verify(strings.Join(parts, ".")); !errors.Is(err, ErrInvalid) {
		t.Fatal("tampered payload accepted")
	}
}
func TestSignedCookieExpirationSizeAndSecureDefaults(t *testing.T) {
	signer, err := New[claims](Options{Name: "session", Keys: []Key{{ID: "a", Secret: bytes.Repeat([]byte{1}, 32)}}, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	signer.now = func() time.Time { return now }
	token, err := signer.Sign(claims{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if err = signer.Set(w, claims{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	c := w.Result().Cookies()[0]
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("unsafe cookie: %+v", c)
	}
	now = now.Add(time.Minute)
	if _, err = signer.Verify(token); !errors.Is(err, ErrInvalid) {
		t.Fatal("expired token accepted")
	}
	if _, err = signer.Sign(claims{strings.Repeat("x", 4000), "b"}); err == nil {
		t.Fatal("oversized value accepted")
	}
	request := httptest.NewRequest("GET", "/", nil)
	request.AddCookie(c)
	request.AddCookie(c)
	if _, err = signer.Read(request); !errors.Is(err, ErrInvalid) {
		t.Fatal("duplicate cookies accepted")
	}
}
func TestSignedCookieCopiesKeys(t *testing.T) {
	secret := bytes.Repeat([]byte{3}, 32)
	signer, err := New[claims](Options{Name: "session", Keys: []Key{{ID: "a", Secret: secret}}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(claims{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range secret {
		secret[i] = 0
	}
	if _, err = signer.Verify(token); err != nil {
		t.Fatal("caller mutated key")
	}
}
func TestSignedCookieRejectsUnsafeOptions(t *testing.T) {
	key := []Key{{ID: "a", Secret: bytes.Repeat([]byte{1}, 32)}}
	for _, tc := range []struct {
		name    string
		options Options
	}{
		{"no keys", Options{Name: "session"}},
		{"short key", Options{Name: "session", Keys: []Key{{ID: "a", Secret: []byte("short")}}}},
		{"insecure host", Options{Name: "__Host-session", Keys: key, Insecure: true}},
		{"host path", Options{Name: "__Host-session", Keys: key, Path: "/else"}},
		{"none insecure", Options{Name: "session", Keys: key, SameSite: http.SameSiteNoneMode, Insecure: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New[claims](tc.options); err == nil {
				t.Fatal("accepted unsafe options")
			}
		})
	}
}
