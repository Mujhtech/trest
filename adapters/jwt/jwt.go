// Package trestjwt verifies bearer JWTs with explicit application-owned keys and
// maps verified claims to a Trest principal. It performs no discovery or key fetch.
package trestjwt

import (
	"context"
	"crypto"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"trest.local/trest"
)

// Options defines one issuer, audience, and signing algorithm for a scope.
// Callbacks must be safe for concurrent requests and honor context cancellation.
type Options[C jwt.Claims] struct {
	// Algorithm is required: HS256/384/512, RS256/384/512, PS256/384/512,
	// ES256/384/512, or EdDSA. The token cannot select a different algorithm.
	Algorithm string
	Issuer    string
	Audience  string
	// NewClaims must return a fresh nonnil pointer or map with no claim defaults.
	// Embed jwt.RegisteredClaims by value in custom claim structs.
	NewClaims func() C
	// Key resolves an untrusted kid (empty if absent) against application-owned
	// verification keys. Do not use kid as a path, URL, or query fragment.
	// HMAC keys must contain at least the algorithm's digest size in random bytes.
	Key func(context.Context, string) (any, error)
	// Principal runs only after signature and claims validation. Return a
	// nonempty ID; application authorization remains a separate policy.
	Principal func(context.Context, C) (trest.Principal, error)
	// Leeway permits clock skew, from zero (default) through five minutes.
	Leeway time.Duration
	// MaxTokenBytes defaults to 16 KiB; explicit values must be 1..64 KiB.
	MaxTokenBytes int
	// Now is an optional test clock. Production applications should leave it nil.
	Now func() time.Time
}

// New constructs scoped authentication without I/O or background work. A missing
// Authorization header leaves the request anonymous; use trest.RequireAuth or a
// permission to protect routes. Present invalid credentials return a uniform 401.
func New[C jwt.Claims](options Options[C]) (trest.Component, error) {
	method, err := signingMethod(options.Algorithm)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.Issuer) == "" || strings.TrimSpace(options.Audience) == "" {
		return nil, errors.New("trestjwt: issuer and audience are required")
	}
	if options.NewClaims == nil || options.Key == nil || options.Principal == nil {
		return nil, errors.New("trestjwt: claims factory, key resolver, and principal mapper are required")
	}
	if options.Leeway < 0 || options.Leeway > 5*time.Minute {
		return nil, errors.New("trestjwt: leeway must be between zero and five minutes")
	}
	if options.MaxTokenBytes == 0 {
		options.MaxTokenBytes = 16 << 10
	}
	if options.MaxTokenBytes < 1 || options.MaxTokenBytes > 64<<10 {
		return nil, errors.New("trestjwt: token limit must be between 1 and 65536 bytes")
	}
	parserOptions := []jwt.ParserOption{
		jwt.WithValidMethods([]string{options.Algorithm}),
		jwt.WithIssuer(options.Issuer), jwt.WithAudience(options.Audience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithLeeway(options.Leeway), jwt.WithStrictDecoding(), jwt.WithJSONNumber(),
	}
	if options.Now != nil {
		parserOptions = append(parserOptions, jwt.WithTimeFunc(options.Now))
	}
	parser := jwt.NewParser(parserOptions...)
	return trest.Authenticate(func(r *http.Request) (trest.Principal, error) {
		ctx := r.Context()
		if err := ctx.Err(); err != nil {
			return trest.Principal{}, err
		}
		var values []string
		present := false
		for name, headers := range r.Header {
			if strings.EqualFold(name, "Authorization") {
				present = true
				values = append(values, headers...)
			}
		}
		if !present {
			return trest.Principal{}, nil
		}
		if len(values) != 1 {
			return trest.Principal{}, rejected(ctx, nil)
		}
		scheme, encoded, ok := strings.Cut(values[0], " ")
		encoded = strings.TrimLeft(encoded, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || encoded == "" || len(encoded) > options.MaxTokenBytes || strings.ContainsAny(encoded, " \t\r\n,") {
			return trest.Principal{}, rejected(ctx, nil)
		}
		claims := options.NewClaims()
		value := reflect.ValueOf(claims)
		if !value.IsValid() || (value.Kind() != reflect.Pointer && value.Kind() != reflect.Map) || value.IsNil() {
			return trest.Principal{}, rejected(ctx, nil)
		}
		token, err := parser.ParseWithClaims(encoded, claims, func(token *jwt.Token) (any, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if token.Header["alg"] != options.Algorithm {
				return nil, errors.New("trestjwt: unexpected algorithm")
			}
			// Only standard JWTs are supported, with no critical extensions.
			if _, exists := token.Header["crit"]; exists {
				return nil, errors.New("trestjwt: unsupported critical header")
			}
			if _, exists := token.Header["b64"]; exists {
				return nil, errors.New("trestjwt: unsupported payload encoding header")
			}
			kid := ""
			if raw, exists := token.Header["kid"]; exists {
				var isString bool
				kid, isString = raw.(string)
				if !isString || len(kid) > 256 || strings.ContainsAny(kid, "\x00\r\n") {
					return nil, errors.New("trestjwt: invalid key identifier")
				}
			}
			// Pin the concrete verifier as well as its name; application-side SDK
			// registration cannot replace this adapter's verification algorithm.
			token.Method = method
			key, err := options.Key(ctx, kid)
			if err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			keyValue := reflect.ValueOf(key)
			if !keyValue.IsValid() || ((keyValue.Kind() == reflect.Pointer || keyValue.Kind() == reflect.Slice) && keyValue.IsNil()) {
				return nil, errors.New("trestjwt: nil verification key")
			}
			if hmac, ok := method.(*jwt.SigningMethodHMAC); ok {
				secret, ok := key.([]byte)
				if !ok || len(secret) < hmac.Hash.Size() {
					return nil, errors.New("trestjwt: invalid hmac verification key")
				}
			}
			return key, nil
		})
		if err != nil || token == nil || !token.Valid {
			return trest.Principal{}, rejected(ctx, err)
		}
		if err := ctx.Err(); err != nil {
			return trest.Principal{}, err
		}
		principal, err := options.Principal(ctx, claims)
		if err != nil || principal.ID == "" || ctx.Err() != nil {
			return trest.Principal{}, rejected(ctx, err)
		}
		return principal, nil
	}), nil
}

func rejected(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return trest.NewError(trest.Unauthenticated, "invalid bearer token", nil)
}

func signingMethod(algorithm string) (jwt.SigningMethod, error) {
	switch algorithm {
	case "HS256":
		return &jwt.SigningMethodHMAC{Name: algorithm, Hash: crypto.SHA256}, nil
	case "HS384":
		return &jwt.SigningMethodHMAC{Name: algorithm, Hash: crypto.SHA384}, nil
	case "HS512":
		return &jwt.SigningMethodHMAC{Name: algorithm, Hash: crypto.SHA512}, nil
	case "RS256":
		return &jwt.SigningMethodRSA{Name: algorithm, Hash: crypto.SHA256}, nil
	case "RS384":
		return &jwt.SigningMethodRSA{Name: algorithm, Hash: crypto.SHA384}, nil
	case "RS512":
		return &jwt.SigningMethodRSA{Name: algorithm, Hash: crypto.SHA512}, nil
	case "PS256":
		return pssMethod(algorithm, crypto.SHA256), nil
	case "PS384":
		return pssMethod(algorithm, crypto.SHA384), nil
	case "PS512":
		return pssMethod(algorithm, crypto.SHA512), nil
	case "ES256":
		return &jwt.SigningMethodECDSA{Name: algorithm, Hash: crypto.SHA256, KeySize: 32, CurveBits: 256}, nil
	case "ES384":
		return &jwt.SigningMethodECDSA{Name: algorithm, Hash: crypto.SHA384, KeySize: 48, CurveBits: 384}, nil
	case "ES512":
		return &jwt.SigningMethodECDSA{Name: algorithm, Hash: crypto.SHA512, KeySize: 66, CurveBits: 521}, nil
	case "EdDSA":
		return &jwt.SigningMethodEd25519{}, nil
	default:
		return nil, fmt.Errorf("trestjwt: unsupported algorithm %q", algorithm)
	}
}

func pssMethod(name string, hash crypto.Hash) jwt.SigningMethod {
	return &jwt.SigningMethodRSAPSS{
		SigningMethodRSA: &jwt.SigningMethodRSA{Name: name, Hash: hash},
		Options:          &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash},
	}
}
