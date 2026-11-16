package operation_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/mujhtech/trest/operation"
)

// These native types need no runtime structural checks, but application hooks
// still own their business rules and must run in the normal enforcement order.
func TestStaticTypeProofPreservesApplicationHooks(t *testing.T) {
	type input struct {
		Value string `json:"value"`
	}
	type output struct {
		Value string `json:"value"`
	}
	cases := []struct {
		name   string
		value  string
		tenant string
		code   operation.Code
		phases []string
	}{
		{"input rejection", "invalid", "owner", operation.InvalidInput, []string{"validate"}},
		{"authorization rejection", "valid", "other", operation.Forbidden, []string{"validate", "authorize"}},
		{"output rejection", "invalid-output", "owner", operation.Internal, []string{"validate", "authorize", "handler", "validate output"}},
		{"success", "valid", "owner", "", []string{"validate", "authorize", "handler", "validate output"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var phases []string
			registry := operation.NewRegistry(nil)
			handle, err := operation.Register(registry, operation.Definition[input, output]{
				ID: "native.hooks.v1", Summary: "Native hooks", Access: operation.Permission("write"),
				Validate: func(_ context.Context, in input) error {
					phases = append(phases, "validate")
					if in.Value == "invalid" {
						return errors.New("business input rejected")
					}
					return nil
				},
				Authorize: func(_ context.Context, principal operation.Principal, _ input) error {
					phases = append(phases, "authorize")
					if principal.TenantID != "owner" {
						return operation.NewError(operation.Forbidden, "not owner", nil)
					}
					return nil
				},
				ValidateOutput: func(_ context.Context, out output) error {
					phases = append(phases, "validate output")
					if out.Value == "invalid-output" {
						return errors.New("business output rejected")
					}
					return nil
				},
			}, func(_ context.Context, in input) (output, error) {
				phases = append(phases, "handler")
				return output(in), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Freeze(); err != nil {
				t.Fatal(err)
			}
			ctx := operation.WithPrincipal(context.Background(), operation.Principal{ID: "verified", TenantID: test.tenant})
			out, err := handle.Invoke(ctx, input{Value: test.value})
			if operation.CodeOf(err) != test.code || !reflect.DeepEqual(phases, test.phases) {
				t.Fatalf("code=%s phases=%v, want code=%s phases=%v", operation.CodeOf(err), phases, test.code, test.phases)
			}
			if test.code != "" && out != (output{}) {
				t.Fatalf("rejected invocation exposed output: %+v", out)
			}
			if test.code == "" && out.Value != test.value {
				t.Fatalf("successful output changed: %+v", out)
			}
		})
	}
}
