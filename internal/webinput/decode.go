package webinput

import (
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/mujhtech/trest/internal/schema"
	"github.com/mujhtech/trest/operation"
)

// Error carries an HTTP parsing status while retaining the shared operation error.
// It never includes request values in its public message.
type Error struct {
	Status int
	Err    error
}

func (e *Error) Error() string {
	if e == nil || e.Err == nil {
		return "invalid request"
	}
	return operation.PublicError(e.Err).Message
}
func (e *Error) Unwrap() error { return e.Err }

// Decode binds and validates one request without writing a response. Its returned
// cleanup must run after the handler, including when Decode fails. Uploaded files
// remain usable until cleanup. The returned value has the exact compiled type.
func (p *Plan) Decode(w http.ResponseWriter, r *http.Request, maxBytes int64) (reflect.Value, func(), error) {
	cleanup := func() {}
	if p == nil || r == nil || maxBytes <= 0 {
		return reflect.Value{}, cleanup, &Error{Status: 500, Err: operation.NewError(operation.Internal, "invalid request binding configuration", nil)}
	}
	out := reflect.New(p.typ).Elem()
	fail := func(err error) (reflect.Value, func(), error) { return reflect.Value{}, cleanup, err }
	var query url.Values
	if r.URL.RawQuery != "" {
		var err error
		query, err = url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return fail(inputError(400, "/query", "encoding", "query string is malformed", err))
		}
		for _, name := range sortedKeys(query) {
			if !p.queries[name] {
				return fail(inputError(400, "/query/"+pointer(name), "unknown_field", "query parameter is not allowed", nil))
			}
		}
	}
	// NoBody is the immutable empty reader. Other readers must remain bounded
	// and probed even when ContentLength says zero or is unknown.
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	if r.ContentLength > maxBytes {
		return fail(inputError(413, "/body", "size", "request body exceeds size limit", nil))
	}
	if p.wholeBody || p.bodyIndex >= 0 {
		if err := requireContentType(r, "application/json"); err != nil {
			return fail(err)
		}
		data, err := readBody(r)
		if err != nil {
			return fail(err)
		}
		if err := validate(p.bodyValidator, data, "/body"); err != nil {
			return fail(err)
		}
		target := out.Addr().Interface()
		if p.bodyIndex >= 0 {
			target = out.Field(p.bodyIndex).Addr().Interface()
		}
		if err := decodeJSON(data, target); err != nil {
			return fail(inputError(400, "/body", "type", "body does not match the request type", err))
		}
	} else if p.form {
		if err := requireContentType(r, p.BodyContentType); err != nil {
			return fail(err)
		}
		if p.multipart {
			// ParseMultipartForm closes its temporary files before returning; cleanup
			// removes names after the handler has finished using FileHeader.Open.
			cleanup = func() {
				if r.MultipartForm != nil {
					_ = r.MultipartForm.RemoveAll()
				}
			}
			if err := r.ParseMultipartForm(64 << 10); err != nil {
				return fail(parseError(err, "/body"))
			}
			for _, name := range sortedKeys(r.MultipartForm.File) {
				if !p.formNames[name] {
					return fail(inputError(400, "/form/"+pointer(name), "unknown_field", "form field is not allowed", nil))
				}
			}
		} else {
			if err := r.ParseForm(); err != nil {
				return fail(parseError(err, "/body"))
			}
		}
		for _, name := range sortedKeys(r.PostForm) {
			if !p.formNames[name] {
				return fail(inputError(400, "/form/"+pointer(name), "unknown_field", "form field is not allowed", nil))
			}
		}
	} else if r.Body != nil && r.Body != http.NoBody {
		var first [1]byte
		n, err := io.ReadFull(r.Body, first[:])
		if err != nil && !errors.Is(err, io.EOF) {
			return fail(parseError(err, "/body"))
		}
		if n > 0 {
			return fail(inputError(400, "/body", "unexpected_body", "this route does not accept a body", nil))
		}
	}
	for _, binding := range p.fields {
		target := out.Field(binding.index)
		if binding.file || binding.manyFiles {
			files := r.MultipartForm.File[binding.name]
			if len(r.PostForm[binding.name]) > 0 {
				return fail(inputError(400, "/form/"+pointer(binding.name), "type", "file field must contain uploaded files", nil))
			}
			if binding.required && len(files) == 0 {
				return fail(inputError(400, "/form/"+pointer(binding.name), "required", "field is required", nil))
			}
			if binding.file {
				if len(files) > 1 {
					return fail(inputError(400, "/form/"+pointer(binding.name), "duplicate_field", "field must appear only once", nil))
				}
				if len(files) == 1 {
					target.Set(reflect.ValueOf(files[0]))
				}
			} else {
				if len(files) < binding.minItems || binding.maxItems >= 0 && len(files) > binding.maxItems {
					return fail(inputError(400, "/form/"+pointer(binding.name), "length", "upload count is outside the allowed range", nil))
				}
				target.Set(reflect.ValueOf(append([]*multipart.FileHeader(nil), files...)))
			}
			continue
		}
		var values []string
		switch binding.source {
		case "path":
			values = []string{r.PathValue(binding.name)}
		case "query":
			values = query[binding.name]
		case "header":
			values = r.Header.Values(binding.name)
			if binding.wireKind == "array" {
				items := make([]string, 0, len(values))
				for _, line := range values {
					for _, item := range strings.Split(line, ",") {
						items = append(items, strings.Trim(item, " \t"))
					}
				}
				values = items
			}
		case "cookie":
			for _, cookie := range r.Cookies() {
				if cookie.Name == binding.name {
					values = append(values, cookie.Value)
				}
			}
		case "form":
			values = r.PostForm[binding.name]
			if p.multipart && len(r.MultipartForm.File[binding.name]) > 0 {
				return fail(inputError(400, "/form/"+pointer(binding.name), "type", "text field cannot contain an uploaded file", nil))
			}
		}
		path := "/" + binding.source + "/" + pointer(binding.name)
		if len(values) == 0 {
			if binding.defaultText != nil {
				values = []string{*binding.defaultText}
			} else if binding.required {
				return fail(inputError(400, path, "required", "field is required", nil))
			} else {
				continue
			}
		}
		wire, err := parameterJSON(values, binding.wireKind, binding.itemKind)
		if err != nil {
			return fail(inputError(400, path, "type", "value does not match the declared type", err))
		}
		if err := validate(binding.validator, wire, path); err != nil {
			return fail(err)
		}
		if err := decodeJSON(wire, target.Addr().Interface()); err != nil {
			return fail(inputError(400, path, "type", "value does not match the declared type", err))
		}
		data, err := encodeJSON(target.Interface())
		if err != nil {
			return fail(inputError(400, path, "type", "value cannot be encoded", err))
		}
		if err := validate(binding.validator, data, path); err != nil {
			return fail(err)
		}
	}
	return out, cleanup, nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func requireContentType(r *http.Request, want string) error {
	kind, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != want {
		return inputError(415, "/body", "content_type", "request Content-Type is not supported", err)
	}
	if charset := parameters["charset"]; charset != "" && !strings.EqualFold(charset, "utf-8") {
		return inputError(415, "/body", "charset", "request charset must be UTF-8", nil)
	}
	return nil
}
func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, inputError(400, "/body", "required", "request body is required", nil)
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, parseError(err, "/body")
	}
	return data, nil
}
func parseError(err error, path string) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return inputError(413, path, "size", "request body exceeds size limit", err)
	}
	return inputError(400, path, "encoding", "request body is malformed", err)
}
func validate(validator *schema.Validator, data []byte, path string) error {
	violations, err := validator.Validate(data)
	if err != nil {
		return inputError(400, path, "encoding", "request JSON is malformed", err)
	}
	if len(violations) == 0 {
		return nil
	}
	fields := make([]operation.FieldError, 0, len(violations))
	for _, v := range violations {
		fields = append(fields, operation.FieldError{Path: path + v.Path, Code: v.Code, Message: v.Message})
	}
	return &Error{Status: 400, Err: &operation.Error{Code: operation.InvalidInput, Message: "request does not match its contract", Fields: fields}}
}
func inputError(status int, path, code, message string, cause error) error {
	return &Error{Status: status, Err: &operation.Error{Code: operation.InvalidInput, Message: message, Fields: []operation.FieldError{{Path: path, Code: code, Message: message}}, Cause: cause}}
}
func pointer(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }
