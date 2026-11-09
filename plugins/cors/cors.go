// Package cors supplies explicit, scoped browser cross-origin access policy.
package cors

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"trest.local/trest"
)

// Options defines a fixed origin allowlist. A wildcard origin is allowed only
// without credentials. CORS supplements authentication; it does not replace it.
type Options struct {
	Origins       []string
	Methods       []string
	Headers       []string
	ExposeHeaders []string
	Credentials   bool
	MaxAge        time.Duration
}

// New creates a scoped component. The app routes OPTIONS preflights through its
// middleware before endpoint authentication; actual requests still authenticate.
func New(options Options) (trest.Component, error) {
	middleware, err := Handler(options)
	if err != nil {
		return nil, err
	}
	return trest.Use(middleware), nil
}

// Handler constructs standard HTTP middleware without starting background work.
// Disallowed origins, methods, or request headers are rejected before the handler.
func Handler(options Options) (trest.Middleware, error) {
	if len(options.Origins) == 0 {
		return nil, errors.New("cors: at least one allowed origin is required")
	}
	origins := make(map[string]bool, len(options.Origins))
	for _, origin := range options.Origins {
		if origin == "*" {
			if options.Credentials {
				return nil, errors.New("cors: wildcard origins cannot allow credentials")
			}
			origins[origin] = true
			continue
		}
		normalized, ok := normalizeOrigin(origin)
		if !ok {
			return nil, errors.New("cors: origins must be complete http or https origins without a path")
		}
		origins[normalized] = true
	}
	if options.MaxAge < 0 || options.MaxAge > 24*time.Hour {
		return nil, errors.New("cors: max age must be between zero and 24 hours")
	}
	methods := make(map[string]bool)
	if len(options.Methods) == 0 {
		options.Methods = []string{http.MethodGet, http.MethodHead}
	}
	for _, method := range options.Methods {
		if !token(method) || strings.ToUpper(method) != method {
			return nil, errors.New("cors: methods must be uppercase HTTP tokens")
		}
		methods[method] = true
	}
	headers := make(map[string]bool)
	for _, header := range options.Headers {
		if !token(header) {
			return nil, errors.New("cors: invalid allowed header")
		}
		headers[strings.ToLower(header)] = true
	}
	exposed := make([]string, 0, len(options.ExposeHeaders))
	for _, header := range options.ExposeHeaders {
		if !token(header) || header == "*" {
			return nil, errors.New("cors: exposed headers must be explicit HTTP names")
		}
		exposed = append(exposed, http.CanonicalHeaderKey(header))
	}
	sort.Strings(exposed)
	allowedMethods := make([]string, 0, len(methods))
	for method := range methods {
		allowedMethods = append(allowedMethods, method)
	}
	sort.Strings(allowedMethods)
	allowedHeaders := make([]string, 0, len(headers))
	for header := range headers {
		allowedHeaders = append(allowedHeaders, http.CanonicalHeaderKey(header))
	}
	sort.Strings(allowedHeaders)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			addVary(w.Header(), "Origin")
			originValues := r.Header.Values("Origin")
			if len(originValues) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			if len(originValues) != 1 {
				deny(w)
				return
			}
			origin := originValues[0]
			normalized, valid := normalizeOrigin(origin)
			if !valid || (!origins["*"] && !origins[normalized]) {
				deny(w)
				return
			}
			preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
			method := r.Method
			if preflight {
				addVary(w.Header(), "Access-Control-Request-Method")
				addVary(w.Header(), "Access-Control-Request-Headers")
				if len(r.Header.Values("Access-Control-Request-Method")) != 1 {
					deny(w)
					return
				}
				method = r.Header.Get("Access-Control-Request-Method")
			}
			if !methods[method] {
				deny(w)
				return
			}
			if preflight {
				for _, line := range r.Header.Values("Access-Control-Request-Headers") {
					for _, header := range strings.Split(line, ",") {
						header = strings.TrimSpace(header)
						if header == "" || !token(header) || !headers[strings.ToLower(header)] {
							deny(w)
							return
						}
					}
				}
			}
			if origins["*"] {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
			if options.Credentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
			if preflight {
				w.Header().Set("Access-Control-Allow-Methods", strings.Join(allowedMethods, ", "))
				if len(allowedHeaders) > 0 {
					w.Header().Set("Access-Control-Allow-Headers", strings.Join(allowedHeaders, ", "))
				}
				if options.MaxAge > 0 {
					w.Header().Set("Access-Control-Max-Age", strconv.FormatInt(int64(options.MaxAge/time.Second), 10))
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if len(exposed) > 0 {
				w.Header().Set("Access-Control-Expose-Headers", strings.Join(exposed, ", "))
			}
			next.ServeHTTP(w, r)
		})
	}, nil
}

func normalizeOrigin(origin string) (string, bool) {
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Host == "" || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.RawPath != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	if strings.ContainsAny(origin, " \t\r\n,") {
		return "", false
	}
	return strings.ToLower(u.Scheme + "://" + u.Host), true
}
func token(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return true
}
func addVary(h http.Header, value string) {
	for _, line := range h.Values("Vary") {
		for _, part := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(part), value) || strings.TrimSpace(part) == "*" {
				return
			}
		}
	}
	h.Add("Vary", value)
}
func deny(w http.ResponseWriter) { http.Error(w, "cross-origin request denied", http.StatusForbidden) }
