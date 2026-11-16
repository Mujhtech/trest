package trest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mujhtech/trest/internal/spanreserve"
	"github.com/mujhtech/trest/operation"
)

func TestRequestSpanReservationSurvivesMiddlewareAndRetainedContexts(t *testing.T) {
	t.Parallel()
	registry := operation.NewRegistry(nil)
	type captured struct {
		ctx     context.Context
		trace   operation.TraceContext
		request string
	}
	var retained []captured
	capture := func(ctx context.Context) {
		retained = append(retained, captured{ctx, operation.TraceFromContext(ctx), operation.CorrelationID(ctx)})
	}
	child, err := operation.Register(registry, operation.Definition[Empty, Empty]{
		ID: "request.child.v1", Summary: "Capture child trace", Access: operation.Public(),
	}, func(ctx context.Context, _ Empty) (Empty, error) {
		capture(ctx)
		return Empty{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := operation.Register(registry, operation.Definition[Empty, Empty]{
		ID: "request.parent.v1", Summary: "Capture parent trace", Access: operation.Public(),
	}, func(ctx context.Context, _ Empty) (Empty, error) {
		capture(ctx)
		return child.Invoke(ctx, Empty{})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Freeze(); err != nil {
		t.Fatal(err)
	}
	var requests []*http.Request
	middleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests = append(requests, r)
			for range 2 {
				if _, err := child.Invoke(r.Context(), Empty{}); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	wrapped := wrapRoute(scope{middleware: []Middleware{middleware}}, RouteInfo{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := parent.Invoke(r.Context(), Empty{}); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	upstream := operation.TraceContext{TraceID: "upstream trace", SpanID: "upstream span"}
	base := operation.WithTrace(context.Background(), upstream)
	var requestIDs []string
	for range 2 {
		original := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(base)
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, original)
		if w.Code != http.StatusOK || original.Context() != base || operation.CorrelationID(base) != "" {
			t.Fatalf("request wrapping changed caller context or failed: %d", w.Code)
		}
		requestIDs = append(requestIDs, w.Header().Get("X-Request-ID"))
	}
	if len(retained) != 8 || requestIDs[0] == "" || requestIDs[0] == requestIDs[1] {
		t.Fatalf("missing or reused request identities: %v, captures=%d", requestIDs, len(retained))
	}
	seen := map[string]bool{requestIDs[0]: true, requestIDs[1]: true}
	for index, value := range retained {
		trace := value.trace
		if trace.SpanID == "" || seen[trace.SpanID] || trace.TraceID != upstream.TraceID || value.request != requestIDs[index/4] {
			t.Fatalf("middleware or nested invocation reused/lost identity: %#v", value)
		}
		seen[trace.SpanID] = true
		wantParent := upstream.SpanID
		if index%4 == 3 {
			wantParent = retained[index-1].trace.SpanID
		}
		if trace.ParentSpanID != wantParent || operation.TraceFromContext(value.ctx) != trace || operation.CorrelationID(value.ctx) != value.request {
			t.Fatalf("later request changed retained metadata or parent link: %#v", value)
		}
	}
	for index, request := range requests {
		if operation.CorrelationID(request.Context()) != requestIDs[index] || operation.TraceFromContext(request.Context()) != upstream || spanreserve.Take(request.Context()) != "" {
			t.Fatal("captured middleware request lost identity or reused its reserved span")
		}
	}
}
