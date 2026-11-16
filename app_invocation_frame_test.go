package trest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mujhtech/trest/internal/invocationctx"
	"github.com/mujhtech/trest/operation"
)

func TestRequestInvocationStorageMatchesRouteAndParent(t *testing.T) {
	type parentValueKey struct{}
	for _, routeKind := range []string{"typed", "observed", "raw"} {
		for _, parentKind := range []string{"background", "cancelable", "wrapped background"} {
			t.Run(routeKind+"/"+parentKind, func(t *testing.T) {
				parent := context.Background()
				switch parentKind {
				case "cancelable":
					var cancel context.CancelFunc
					parent, cancel = context.WithCancel(parent)
					defer cancel()
				case "wrapped background":
					parent = context.WithValue(parent, parentValueKey{}, "parent")
				}
				called := false
				check := func(ctx context.Context) {
					called = true
					state, _ := invocationctx.RequestState(ctx).(*requestState)
					if state == nil {
						t.Fatal("request state missing")
					}
					if routeKind != "typed" {
						if state.invocation != nil {
							t.Fatal("route reserved unused invocation storage")
						}
					} else if parentKind == "background" {
						if _, ok := state.invocation.(*invocationctx.Slot); !ok {
							t.Fatal("background request did not reserve lazy storage")
						}
					} else if _, ok := state.invocation.(*invocationctx.TimerSlot); !ok {
						t.Fatal("request did not reserve compact timer storage")
					}
				}
				var route Component = Get("/", func(ctx context.Context, _ Empty) (string, error) {
					check(ctx)
					return "ok", nil
				})
				if routeKind == "raw" {
					route = Raw(http.MethodGet, "/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						check(r.Context())
						w.WriteHeader(http.StatusOK)
					}))
				}
				components := []Component{route}
				if routeKind == "observed" {
					components = append(components, Observe(operation.ObserverFunc(func(context.Context, operation.Event) {})))
				}
				app, err := New(components...)
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				app.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(parent))
				if !called || w.Code != http.StatusOK {
					t.Fatalf("handler=%v response=%d %s", called, w.Code, w.Body)
				}
			})
		}
	}
}

type panicLifetimeContext struct{ context.Context }

func (panicLifetimeContext) Deadline() (time.Time, bool) { panic("parent Deadline") }
func (panicLifetimeContext) Done() <-chan struct{}       { panic("parent Done") }

func TestRequestStorageDoesNotProbeParentBeforeBindingOrRecovery(t *testing.T) {
	type body struct {
		Value string `json:"value"`
	}
	for _, input := range []string{"invalid JSON", `{"value":"ok"}`} {
		t.Run(input, func(t *testing.T) {
			called := false
			app, err := New(Post("/", func(context.Context, body) (string, error) {
				called = true
				return "ok", nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(input))
			request.Header.Set("Content-Type", "application/json")
			request = request.WithContext(panicLifetimeContext{Context: context.Background()})
			w := httptest.NewRecorder()
			app.ServeHTTP(w, request)
			want := http.StatusBadRequest
			if input != "invalid JSON" {
				want = http.StatusInternalServerError
			}
			if called || w.Code != want {
				t.Fatalf("handler=%v status=%d, want %d", called, w.Code, want)
			}
		})
	}
}
