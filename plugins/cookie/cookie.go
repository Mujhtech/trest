// Package cookie signs bounded typed cookie values with rotating HMAC keys.
// Signatures provide integrity, not encryption: cookie contents remain readable.
package cookie

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid is returned for absent, expired, malformed, or unauthenticated data.
var ErrInvalid = errors.New("cookie: invalid or expired signed value")

// Key is an application-owned random secret. Use at least 32 random bytes.
// IDs identify rotation generations and must not contain punctuation other than - or _.
type Key struct {
	ID     string
	Secret []byte
}

// Options configures a single host-only cookie. The first key signs new values;
// all configured keys verify existing values. No keys or state are persisted.
type Options struct {
	Name     string
	Keys     []Key
	Path     string
	TTL      time.Duration
	SameSite http.SameSite
	// Insecure permits HTTP for explicitly configured local development only.
	Insecure bool
	// MaxBytes caps the complete signed value, not just its JSON body. Default 3500.
	MaxBytes int
}

// Signer is immutable after construction and safe for concurrent requests.
type Signer[T any] struct {
	options Options
	keys    map[string][]byte
	now     func() time.Time
}

// New validates and copies key material. Cookies default to Secure, HttpOnly,
// SameSite=Lax, host-only, path=/, and a one-hour lifetime.
func New[T any](options Options) (*Signer[T], error) {
	if options.Path == "" {
		options.Path = "/"
	}
	if options.TTL == 0 {
		options.TTL = time.Hour
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = 3500
	}
	if options.SameSite == 0 {
		options.SameSite = http.SameSiteLaxMode
	}
	if options.Name == "" || options.TTL < time.Second || options.TTL > 365*24*time.Hour || options.MaxBytes < 128 || options.MaxBytes > 4096 {
		return nil, errors.New("cookie: invalid name, lifetime, or size limit")
	}
	if options.Path[0] != '/' || strings.ContainsAny(options.Path, ";\r\n") {
		return nil, errors.New("cookie: path must be an absolute URL path")
	}
	if options.SameSite != http.SameSiteLaxMode && options.SameSite != http.SameSiteStrictMode && options.SameSite != http.SameSiteNoneMode {
		return nil, errors.New("cookie: invalid SameSite policy")
	}
	if options.SameSite == http.SameSiteNoneMode && options.Insecure {
		return nil, errors.New("cookie: SameSite=None requires Secure")
	}
	if strings.HasPrefix(options.Name, "__Host-") && (options.Insecure || options.Path != "/") {
		return nil, errors.New("cookie: __Host- cookies require Secure and path=/")
	}
	if strings.HasPrefix(options.Name, "__Secure-") && options.Insecure {
		return nil, errors.New("cookie: __Secure- cookies require Secure")
	}
	if err := (&http.Cookie{Name: options.Name, Value: "value", Path: options.Path, Secure: !options.Insecure, HttpOnly: true, SameSite: options.SameSite}).Valid(); err != nil {
		return nil, errors.New("cookie: invalid cookie configuration")
	}
	if len(options.Keys) == 0 || len(options.Keys) > 16 {
		return nil, errors.New("cookie: supply between one and sixteen keys")
	}
	keys := make(map[string][]byte, len(options.Keys))
	copied := make([]Key, len(options.Keys))
	for i, key := range options.Keys {
		if !validID(key.ID) || len(key.Secret) < 32 {
			return nil, errors.New("cookie: each key needs a valid ID and at least 32 bytes")
		}
		if _, ok := keys[key.ID]; ok {
			return nil, errors.New("cookie: duplicate key ID")
		}
		secret := append([]byte(nil), key.Secret...)
		keys[key.ID] = secret
		copied[i] = Key{ID: key.ID, Secret: secret}
	}
	options.Keys = copied
	return &Signer[T]{options: options, keys: keys, now: time.Now}, nil
}

// Sign returns a value with a fixed expiration. Never put secrets in the payload:
// the signature authenticates bytes but does not hide their contents.
func (s *Signer[T]) Sign(value T) (string, error) {
	token, _, err := s.sign(value)
	return token, err
}
func (s *Signer[T]) sign(value T) (string, time.Time, error) {
	if s == nil {
		return "", time.Time{}, ErrInvalid
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := s.now().Add(s.options.TTL).Truncate(time.Second)
	prefix := "v1." + s.options.Keys[0].ID + "." + strconv.FormatInt(expires.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(data)
	result := prefix + "." + base64.RawURLEncoding.EncodeToString(s.mac(s.options.Keys[0].Secret, prefix))
	if len(result) > s.options.MaxBytes {
		return "", time.Time{}, errors.New("cookie: signed value exceeds size limit")
	}
	return result, expires, nil
}

// Verify authenticates the complete envelope before parsing its JSON payload.
func (s *Signer[T]) Verify(value string) (T, error) {
	var zero T
	if s == nil || len(value) > s.options.MaxBytes {
		return zero, ErrInvalid
	}
	parts := strings.Split(value, ".")
	if len(parts) != 5 || parts[0] != "v1" {
		return zero, ErrInvalid
	}
	key, ok := s.keys[parts[1]]
	if !ok {
		return zero, ErrInvalid
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(signature) != sha256.Size {
		return zero, ErrInvalid
	}
	prefix := strings.Join(parts[:4], ".")
	if !hmac.Equal(signature, s.mac(key, prefix)) {
		return zero, ErrInvalid
	}
	expires, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || expires <= s.now().Unix() {
		return zero, ErrInvalid
	}
	// A trusted issuer must not accidentally create effectively unbounded values.
	if expires > s.now().Add(s.options.TTL+time.Second).Unix() {
		return zero, ErrInvalid
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(parts[3])
	if err != nil {
		return zero, ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var decoded T
	if decoder.Decode(&decoded) != nil {
		return zero, ErrInvalid
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return zero, ErrInvalid
	}
	return decoded, nil
}

// Set signs a value and adds a secure cookie to the response.
func (s *Signer[T]) Set(w http.ResponseWriter, value T) error {
	token, expires, err := s.sign(value)
	if err != nil {
		return err
	}
	c := s.template()
	c.Value = token
	c.Expires = expires
	c.MaxAge = int(s.options.TTL / time.Second)
	http.SetCookie(w, c)
	return nil
}

// Read requires exactly one matching cookie so conflicting path/domain cookies
// cannot silently select a different authenticated session.
func (s *Signer[T]) Read(r *http.Request) (T, error) {
	var zero T
	if s == nil || r == nil {
		return zero, ErrInvalid
	}
	var value string
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == s.options.Name {
			count++
			value = c.Value
		}
	}
	if count != 1 {
		return zero, ErrInvalid
	}
	return s.Verify(value)
}

// Delete expires the cookie using the same host, path, and security attributes.
func (s *Signer[T]) Delete(w http.ResponseWriter) {
	if s == nil {
		return
	}
	c := s.template()
	c.MaxAge = -1
	c.Expires = time.Unix(1, 0)
	http.SetCookie(w, c)
}
func (s *Signer[T]) template() *http.Cookie {
	return &http.Cookie{Name: s.options.Name, Path: s.options.Path, HttpOnly: true, Secure: !s.options.Insecure, SameSite: s.options.SameSite}
}
func (s *Signer[T]) mac(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte("trest/signed-cookie/v1\x00" + s.options.Name + "\x00" + s.options.Path + "\x00" + value))
	return h.Sum(nil)
}
func validID(id string) bool {
	if len(id) < 1 || len(id) > 32 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}
