package trest

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"trest.local/trest/internal/schema"
)

// Response carries a typed JSON body and an explicitly declared HTTP status.
// Declare allowed statuses with Route.Status or Route.Statuses. Unlike Reply,
// its body contributes to runtime validation, OpenAPI, and generated clients.
type Response[T any] struct {
	body   T
	status int
	header http.Header
	err    error
}

// JSON returns a typed HTTP response. An undeclared status is rejected before
// delivery. Use ordinary return values when the route has one fixed status.
func JSON[T any](status int, body T) Response[T] { return Response[T]{body: body, status: status} }

// WithHeader returns a response copy with an additional header.
func (r Response[T]) WithHeader(name, value string) Response[T] {
	r.header, r.err = responseHeader(r.header, name, value, r.err)
	return r
}

// Either represents one of two typed JSON response bodies. First and Second
// choose the body; Route.Statuses declares the corresponding two HTTP statuses.
// Its zero value is invalid so an accidental missing branch cannot be sent.
type Either[A, B any] struct {
	first  A
	second B
	branch int
	header http.Header
	err    error
}

// First selects the first body and first status declared by Route.Statuses.
func First[A, B any](body A) Either[A, B] { return Either[A, B]{first: body, branch: 1} }

// Second selects the second body and second status declared by Route.Statuses.
func Second[A, B any](body B) Either[A, B] { return Either[A, B]{second: body, branch: 2} }

// WithHeader returns a result copy with an additional header.
func (r Either[A, B]) WithHeader(name, value string) Either[A, B] {
	r.header, r.err = responseHeader(r.header, name, value, r.err)
	return r
}

type typedResponse interface {
	responseTypes() []reflect.Type
	responseValue() (int, int, any, http.Header, error)
}

func (r Response[T]) responseTypes() []reflect.Type { return []reflect.Type{reflect.TypeFor[T]()} }
func (r Response[T]) responseValue() (int, int, any, http.Header, error) {
	return -1, r.status, r.body, r.header, r.err
}
func (r Either[A, B]) responseTypes() []reflect.Type {
	return []reflect.Type{reflect.TypeFor[A](), reflect.TypeFor[B]()}
}
func (r Either[A, B]) responseValue() (int, int, any, http.Header, error) {
	if r.branch == 1 {
		return 0, 0, r.first, r.header, r.err
	}
	if r.branch == 2 {
		return 1, 0, r.second, r.header, r.err
	}
	return -1, 0, nil, nil, errors.New("trest: typed response has no selected branch")
}
func responseHeader(header http.Header, name, value string, previous error) (http.Header, error) {
	if previous != nil {
		return header, previous
	}
	if !validHeaderName(name) || strings.ContainsAny(value, "\r\n") {
		return header, errors.New("trest: invalid response header")
	}
	if header == nil {
		header = make(http.Header)
	} else {
		header = header.Clone()
	}
	header.Set(name, value)
	return header, nil
}

// ResponseInfo describes one declared typed HTTP success response.
type ResponseInfo struct {
	Status      int             `json:"status"`
	ContentType string          `json:"content_type,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

func isResponseWrapper(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == reflect.TypeFor[Reply]() || t.Implements(reflect.TypeFor[typedResponse]()) || t.Implements(reflect.TypeFor[typedEventStream]())
}

func typedResponses(t reflect.Type, statuses []int, fallback int) ([]ResponseInfo, []*schema.Validator, error) {
	zero, ok := reflect.Zero(t).Interface().(typedResponse)
	if !ok {
		if len(statuses) > 0 {
			return nil, nil, errors.New("trest: Statuses requires Response or Either output")
		}
		return nil, nil, nil
	}
	types := zero.responseTypes()
	if len(statuses) == 0 {
		if len(types) > 1 {
			return nil, nil, errors.New("trest: Either requires two declared Statuses")
		}
		statuses = []int{fallback}
	}
	if len(types) > 1 && len(statuses) != len(types) {
		return nil, nil, errors.New("trest: each response branch requires one status")
	}
	seen := map[int]bool{}
	var responses []ResponseInfo
	var validators []*schema.Validator
	for i, status := range statuses {
		if status < 200 || status > 399 || seen[status] {
			return nil, nil, fmt.Errorf("trest: invalid or duplicate response status %d", status)
		}
		seen[status] = true
		index := i
		if len(types) == 1 {
			index = 0
		}
		if isResponseWrapper(types[index]) {
			return nil, nil, errors.New("trest: a typed response body must be a JSON value, not another response")
		}
		shape, err := schema.Generate(types[index])
		if err != nil {
			return nil, nil, err
		}
		validator, err := schema.Compile(shape)
		if err != nil {
			return nil, nil, err
		}
		response := ResponseInfo{Status: status, ContentType: "application/json", Schema: shape}
		if status == 204 || status == 304 {
			response.ContentType = ""
			response.Schema = nil
		}
		responses = append(responses, response)
		validators = append(validators, validator)
	}
	return responses, validators, nil
}

func selectResponse(reply typedResponse, responses []ResponseInfo) (int, any, http.Header, error) {
	branch, status, body, headers, err := reply.responseValue()
	if err != nil {
		return 0, nil, nil, err
	}
	if branch >= 0 {
		if branch >= len(responses) {
			return 0, nil, nil, errors.New("trest: undeclared response branch")
		}
		return branch, body, headers, nil
	}
	for i, response := range responses {
		if response.Status == status {
			return i, body, headers, nil
		}
	}
	return 0, nil, nil, fmt.Errorf("trest: undeclared response status %d", status)
}
