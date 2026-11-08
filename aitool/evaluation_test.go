package aitool

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
)

// TestRecordedEvaluation replays proposed tool calls from a versioned fixture.
// It makes no provider calls, and its only application handler is an in-memory
// counter that must remain zero because every valid write requires approval.
func TestRecordedEvaluation(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/evaluation.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtureData struct {
		SchemaVersion int    `json:"schema_version"`
		PromptVersion string `json:"prompt_version"`
		ToolContract  string `json:"tool_contract"`
		Cases         []struct {
			Name       string          `json:"name"`
			Tool       string          `json:"tool"`
			Arguments  json.RawMessage `json:"arguments"`
			WantStatus State           `json:"want_status"`
			WantError  string          `json:"want_error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixtureData); err != nil {
		t.Fatal(err)
	}
	if fixtureData.SchemaVersion != 1 || fixtureData.PromptVersion != "resource-assistant.v1" || fixtureData.ToolContract != "test.write.v1" {
		t.Fatal("fixture versions must be reviewed explicitly")
	}
	for _, tc := range fixtureData.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			var calls atomic.Int64
			_, s := fixture(t, NewMemoryStore(), true, func(context.Context, input) (output, error) { calls.Add(1); return output{}, nil }, nil)
			modelCall, err := s.ReserveModelCall(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.CompleteModelCall(modelCall, "fixture-v1", 0); err != nil {
				t.Fatal(err)
			}
			result := s.Call(context.Background(), tc.Tool, tc.Arguments)
			if result.Status != tc.WantStatus {
				t.Fatalf("status %q, want %q", result.Status, tc.WantStatus)
			}
			gotError := ""
			if result.Failure != nil {
				gotError = result.Failure.Code
			}
			if gotError != tc.WantError {
				t.Fatalf("code %q, want %q", gotError, tc.WantError)
			}
			if calls.Load() != 0 {
				t.Fatal("evaluation performed an unapproved side effect")
			}
		})
	}
}
