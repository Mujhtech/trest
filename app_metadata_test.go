package trest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mujhtech/trest/operation"
)

func TestRequestMetadataAndHeaderStorageRemainRequestOwned(t *testing.T) {
	t.Parallel()
	type applicationKey struct{}
	parent, cancel := context.WithCancelCause(context.WithValue(context.Background(), applicationKey{}, "first request"))
	defer cancel(nil)
	var requests []*http.Request
	var contexts []context.Context
	var idHeaders, contentTypes [][]string
	wrapped := wrapRoute(scope{}, RouteInfo{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r)
		contexts = append(contexts, r.Context())
		setResponseContentType(w, r, "application/json; charset=utf-8")
		idHeaders = append(idHeaders, w.Header()["X-Request-Id"])
		contentTypes = append(contentTypes, w.Header()["Content-Type"])
		w.WriteHeader(http.StatusOK)
	}))
	first := httptest.NewRecorder()
	firstRequest := httptest.NewRequest(http.MethodGet, "/first", nil).WithContext(parent)
	wrapped.ServeHTTP(first, firstRequest)
	if requests[0] == firstRequest || firstRequest.Context() != parent || operation.CorrelationID(firstRequest.Context()) != "" {
		t.Fatal("request wrapping changed the caller's request or context")
	}
	firstID := operation.CorrelationID(contexts[0])
	if firstID == "" || idHeaders[0][0] != firstID {
		t.Fatalf("first request metadata/header mismatch: %q %v", firstID, idHeaders[0])
	}
	second := httptest.NewRecorder()
	secondRequest := httptest.NewRequest(http.MethodPost, "/second", nil)
	secondContext := secondRequest.Context()
	wrapped.ServeHTTP(second, secondRequest)
	if requests[1] == secondRequest || secondRequest.Context() != secondContext || operation.CorrelationID(secondRequest.Context()) != "" {
		t.Fatal("second request wrapping changed the caller's request or context")
	}
	secondID := operation.CorrelationID(contexts[1])
	if secondID == "" || secondID == firstID || idHeaders[1][0] != secondID {
		t.Fatalf("second request reused metadata/header: %q %v", secondID, idHeaders[1])
	}
	if operation.CorrelationID(contexts[0]) != firstID || idHeaders[0][0] != firstID || contexts[0].Value(applicationKey{}) != "first request" {
		t.Fatal("later request changed retained context or header storage")
	}
	if requests[0] == requests[1] || requests[0].Context() != contexts[0] || requests[0].Method != http.MethodGet || requests[0].RequestURI != "/first" || requests[1].Context() != contexts[1] || requests[1].Method != http.MethodPost || requests[1].RequestURI != "/second" {
		t.Fatal("later request changed a retained request or its context")
	}
	// Header slices remain application-owned and mutable. Their contents must
	// not share backing storage with another request or the immutable context ID.
	idHeaders[0][0] = "changed by middleware"
	contentTypes[0][0] = "text/plain"
	if operation.CorrelationID(contexts[0]) != firstID || idHeaders[1][0] != secondID || contentTypes[1][0] != "application/json; charset=utf-8" {
		t.Fatal("header mutation changed immutable metadata or another response")
	}
	cause := errors.New("caller canceled")
	cancel(cause)
	if contexts[0].Err() != context.Canceled || !errors.Is(context.Cause(contexts[0]), cause) || contexts[1].Err() != nil {
		t.Fatal("retained contexts lost their independent cancellation lifetime")
	}
}

func TestContentTypeReplacementPreservesRetainedHeaderSlices(t *testing.T) {
	t.Parallel()
	wrapped := wrapRoute(scope{}, RouteInfo{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setResponseContentType(w, r, "application/json")
		first := w.Header()["Content-Type"]
		setResponseContentType(w, r, "text/plain")
		second := w.Header()["Content-Type"]
		setResponseContentType(w, r, "application/octet-stream")
		if first[0] != "application/json" || second[0] != "text/plain" || w.Header().Get("Content-Type") != "application/octet-stream" {
			t.Fatal("replacement mutated a retained header value")
		}
		first[0], second[0] = "changed first", "changed second"
		if w.Header().Get("Content-Type") != "application/octet-stream" {
			t.Fatal("replacement shares backing storage with a previous header")
		}
	}))
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestContentTypeOutsideRequestStateUsesNormalHeaderSemantics(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	setResponseContentType(w, r, "application/json")
	first := w.Header()["Content-Type"]
	setResponseContentType(w, r, "text/plain")
	if first[0] != "application/json" || w.Header().Get("Content-Type") != "text/plain" {
		t.Fatal("fallback changed standard Header.Set replacement semantics")
	}
}
