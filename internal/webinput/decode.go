package webinput

import (
	"bytes"
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
	if p == nil || r == nil || maxBytes <= 0 {
		return reflect.Value{}, func() {}, bindingConfigurationError()
	}
	return p.decode(w, r, maxBytes, reflect.New(p.typ).Elem(), false)
}

// DecodeInto binds into a pointer to the compiled input type. It clears the
// target before binding, preserving Decode's fresh-value semantics when callers
// reuse a destination. The value must not be used when an error is returned.
// This lets typed routes avoid copying a reflect.Value into a second interface
// allocation before invocation. Parsing, validation, and cleanup are shared with
// Decode, including original-wire checks before custom decoders execute.
func (p *Plan) DecodeInto(w http.ResponseWriter, r *http.Request, maxBytes int64, target any) (func(), error) {
	return p.decodeInto(w, r, maxBytes, target, false)
}

// CanDecodeReadOnly reports whether binding can consume a request without
// changing its fields. Form parsing requires the ordinary mutating path.
func (p *Plan) CanDecodeReadOnly() bool { return p != nil && !p.form }

// DecodeIntoReadOnly preserves the request's fields, including its Body and
// Context, while binding into a cleared pointer to the compiled input type.
// It consumes the underlying body normally using a local bounded reader. Form
// plans return a configuration error before consuming or modifying anything.
// Validation, error reporting, and target ownership match DecodeInto.
func (p *Plan) DecodeIntoReadOnly(w http.ResponseWriter, r *http.Request, maxBytes int64, target any) (func(), error) {
	return p.decodeInto(w, r, maxBytes, target, true)
}

func (p *Plan) decodeInto(w http.ResponseWriter, r *http.Request, maxBytes int64, target any, readOnly bool) (func(), error) {
	if p == nil || r == nil || maxBytes <= 0 || readOnly && p.form {
		return func() {}, bindingConfigurationError()
	}
	value := reflect.ValueOf(target)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Type() != p.typ {
		return func() {}, bindingConfigurationError()
	}
	out := value.Elem()
	out.SetZero()
	_, cleanup, err := p.decode(w, r, maxBytes, out, readOnly)
	return cleanup, err
}

func bindingConfigurationError() error {
	return &Error{Status: 500, Err: operation.NewError(operation.Internal, "invalid request binding configuration", nil)}
}

func (p *Plan) decode(w http.ResponseWriter, r *http.Request, maxBytes int64, out reflect.Value, readOnly bool) (reflect.Value, func(), error) {
	cleanup := func() {}
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
	body := r.Body
	if body != nil && body != http.NoBody {
		body = http.MaxBytesReader(w, body, maxBytes)
		if !readOnly {
			r.Body = body
		}
	}
	if r.ContentLength > maxBytes {
		return fail(inputError(413, "/body", "size", "request body exceeds size limit", nil))
	}
	if p.wholeBody || p.bodyIndex >= 0 {
		if err := requireContentType(r, "application/json"); err != nil {
			return fail(err)
		}
		var data []byte
		var err error
		if p.bodyPool != nil && body != nil {
			buffer := p.bodyPool.Get().(*bytes.Buffer)
			defer p.releaseBody(buffer)
			_, err = buffer.ReadFrom(body)
			if err != nil {
				err = parseError(err, "/body")
			}
			data = buffer.Bytes()
		} else {
			data, err = readBody(body)
		}
		if err != nil {
			return fail(err)
		}
		target := out
		if p.bodyIndex >= 0 {
			target = out.Field(p.bodyIndex)
		}
		if !p.bodyDecoder.TryDecode(data, target) {
			// A declined native plan may have assigned earlier fields. Preserve
			// fresh decoding and the existing complete error diagnostics.
			target.SetZero()
			if err := validate(p.bodyValidator, data, "/body"); err != nil {
				return fail(err)
			}
			if err := decodeJSON(data, target.Addr().Interface()); err != nil {
				return fail(inputError(400, "/body", "type", "body does not match the request type", err))
			}
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
	} else if body != nil && body != http.NoBody {
		var first [1]byte
		n, err := io.ReadFull(body, first[:])
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
				return fail(inputError(400, binding.path, "type", "file field must contain uploaded files", nil))
			}
			if binding.required && len(files) == 0 {
				return fail(inputError(400, binding.path, "required", "field is required", nil))
			}
			if binding.file {
				if len(files) > 1 {
					return fail(inputError(400, binding.path, "duplicate_field", "field must appear only once", nil))
				}
				if len(files) == 1 {
					target.Set(reflect.ValueOf(files[0]))
				}
			} else {
				if len(files) < binding.minItems || binding.maxItems >= 0 && len(files) > binding.maxItems {
					return fail(inputError(400, binding.path, "length", "upload count is outside the allowed range", nil))
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
				return fail(inputError(400, binding.path, "type", "text field cannot contain an uploaded file", nil))
			}
		}
		path := binding.path
		if len(values) == 0 {
			if binding.defaultText != nil {
				values = []string{*binding.defaultText}
			} else if binding.required {
				return fail(inputError(400, path, "required", "field is required", nil))
			} else {
				continue
			}
		}
		if len(values) == 1 && binding.decoder != nil {
			// Method-free native scalars preserve the validated wire value on
			// re-encoding. They need one validation/assignment pass; codecs,
			// optional pointers, and collections retain both wire checks below.
			if binding.decoder.TryDecodeParameter(values[0], target) {
				continue
			}
			// A declined scalar leaves target unchanged. The general path
			// retains the complete public error diagnostics.
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

func (p *Plan) releaseBody(buffer *bytes.Buffer) {
	// The value decoder and its standard-library fallback own every decoded
	// string. Clear request data before returning small storage to this plan.
	clear(buffer.Bytes())
	if buffer.Cap() > 32<<10 {
		*buffer = bytes.Buffer{}
	} else {
		buffer.Reset()
	}
	p.bodyPool.Put(buffer)
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
	contentType := r.Header.Get("Content-Type")
	// The exact common value has no parameters to inspect. All other spellings,
	// charsets, and malformed values retain the standard MIME parser's behavior.
	if contentType == want {
		return nil
	}
	kind, parameters, err := mime.ParseMediaType(contentType)
	if err != nil || kind != want {
		return inputError(415, "/body", "content_type", "request Content-Type is not supported", err)
	}
	if charset := parameters["charset"]; charset != "" && !strings.EqualFold(charset, "utf-8") {
		return inputError(415, "/body", "charset", "request charset must be UTF-8", nil)
	}
	return nil
}
func readBody(body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, inputError(400, "/body", "required", "request body is required", nil)
	}
	data, err := io.ReadAll(body)
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
