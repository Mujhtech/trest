# JWT authentication adapter

This optional Go 1.23 module uses [golang-jwt/jwt v5.3.1](https://github.com/golang-jwt/jwt/releases/tag/v5.3.1). It keeps JWT dependencies outside Trest's standard-library-only core. Construction performs no I/O and starts no goroutines.

```go
type Claims struct {
    jwt.RegisteredClaims
    Tenant string `json:"tenant"`
}

authentication, err := trestjwt.New(trestjwt.Options[*Claims]{
    Algorithm: "EdDSA",
    Issuer:    "https://identity.example.com",
    Audience:  "invoice-api",
    NewClaims: func() *Claims { return &Claims{} },
    Key: func(ctx context.Context, kid string) (any, error) {
        return keys.Lookup(ctx, kid) // application-owned verification key store
    },
    Principal: func(ctx context.Context, claims *Claims) (trest.Principal, error) {
        return trest.Principal{ID: claims.Subject, TenantID: claims.Tenant}, nil
    },
})
if err != nil {
    return err
}
app, err := trest.New(
    trest.Group("/api", authentication, trest.RequireAuth(), routes),
)
```

Import the adapter as `trestjwt "trest.local/trest/adapters/jwt"` and the claim types from `github.com/golang-jwt/jwt/v5`. The module path remains provisional. The local example below builds from this checkout.

`Algorithm`, `Issuer`, `Audience`, `NewClaims`, `Key`, and `Principal` are required. Exactly one configured algorithm is accepted: HS256/384/512, RS256/384/512, PS256/384/512, ES256/384/512, or EdDSA. Unsigned tokens and custom algorithms are rejected. The verifier requires a valid signature, an expiration, the exact issuer, and an audience containing the configured value. It checks `nbf` and `iat` when present. Unsupported critical or payload-encoding headers are rejected. `Principal` runs only after these checks, and an empty principal ID is rejected.

`NewClaims` must allocate a new nonnil pointer or map for each request, with no prefilled claim values. Embed `jwt.RegisteredClaims` by value. Claim getters, optional `Validate` methods, and custom JSON decoding are trusted application code; they must preserve registered-claim semantics. Callbacks must be concurrency-safe and honor cancellation. The `Now` option exists for deterministic tests; production applications should use the default system clock.

The application owns keys, rotation, identity mapping, and authorization. `Key` receives only an untrusted `kid`, at most 256 bytes, or an empty string if absent. Resolve it against an explicit trusted key collection; never interpret it as a URL, file path, or database fragment. Return the SDK's expected verification key type: `[]byte` for HMAC, `*rsa.PublicKey`, `*ecdsa.PublicKey`, or `ed25519.PublicKey`. HMAC keys require at least 32/48/64 random bytes for HS256/384/512. Treat installed keys as immutable during use; synchronize application-owned rotation. This adapter never follows token-supplied key URLs or performs discovery or refresh.

A missing Authorization header is anonymous access. Add `trest.RequireAuth()` or a route permission to require identity. A present malformed, duplicate, invalid, or rejected bearer credential returns the same 401 message, including when the route is public. Callback error details are not exposed. Request cancellation and callback cancellation/deadline errors keep their cancellation classification. JWT authentication does not grant resource permissions; continue to apply application-owned guards and authorization. Revocation, session management, token issuance, OAuth/OIDC flows, and identity-provider administration are outside this adapter.

The default token limit is 16 KiB, configurable from 1 through 64 KiB. Clock leeway defaults to zero and is bounded at five minutes. Tokens must use unpadded canonical base64url encoding. Only the Authorization bearer header is read; query strings and cookies are not authentication sources.

## Local example

```sh
cd adapters/jwt
go run ./cmd/demo
```

The demo explicitly generates a synthetic random signing key in memory, prints one five-minute local token, and listens on `127.0.0.1:8083`. In another terminal, copy that token into `TOKEN`:

```sh
TOKEN='paste-the-local-demo-token'
curl http://127.0.0.1:8083/me -H "Authorization: Bearer $TOKEN"
```

The response contains the verified `demo-user` identity and `acme` tenant. Without the header the endpoint returns 401. The demo creates no accounts, uses no external identity provider, and stores no key or token on disk. Restarting replaces its key. Printing credentials is limited to this explicitly local fixture.

```sh
go test -race ./...
go vet ./...
go build ./...
```

Tests cover typed identity propagation, rejection before principal mapping and handler execution, issuer/audience/expiry/not-before/issued-at enforcement, signature and algorithm mismatch, malformed and oversized headers, key rotation, cancellation, strict decoding, and concurrent claim isolation. They use synthetic keys and `httptest`; no production credentials or network service is needed.
