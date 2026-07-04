package runtime

import (
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/humanrequest"
)

func TestAnyString(t *testing.T) {
	if got := anyString("  hi  "); got != "hi" {
		t.Fatalf("string: got %q", got)
	}
	if got := anyString(123); got != "" {
		t.Fatalf("default arm should be empty, got %q", got)
	}
	// fmt.Stringer path.
	if got := anyString(strings.ToLower("ABC")); got != "abc" {
		t.Fatalf("Stringer path: got %q", got)
	}
}

// TestInterruptReason covers empty / non-empty.
func TestInterruptReason(t *testing.T) {
	if got := interruptReason(nil); got != "" {
		t.Fatalf("empty should be empty")
	}
	if got := interruptReason([]BlockedBy{{Type: "approval"}}); got != "approval" {
		t.Fatalf("got %q", got)
	}
}

// TestRequestKindFromString covers freeform / approval / unsupported arms.
func TestRequestKindFromString(t *testing.T) {
	if k, err := requestKindFromString("freeform"); err != nil || k != humanrequest.RequestFreeform {
		t.Fatalf("freeform: k=%v err=%v", k, err)
	}
	if k, err := requestKindFromString("approval"); err != nil || k != humanrequest.RequestApproval {
		t.Fatalf("approval: k=%v err=%v", k, err)
	}
	if _, err := requestKindFromString("bogus"); err == nil {
		t.Fatalf("unsupported should error")
	}
}

// TestCloneAnyMapDeep covers empty / roundtrip-deep-clone.
func TestCloneAnyMapDeep(t *testing.T) {
	if got := cloneAnyMapDeep(nil); got != nil {
		t.Fatalf("nil should be nil")
	}
	if got := cloneAnyMapDeep(map[string]any{}); got != nil {
		t.Fatalf("empty should be nil")
	}
	src := map[string]any{"a": 1, "nested": map[string]any{"b": 2}}
	clone := cloneAnyMapDeep(src)
	// JSON roundtrip turns int into float64; compare by numeric value.
	if a, ok := clone["a"].(float64); !ok || a != 1 {
		t.Fatalf("clone missing key a: %v", clone["a"])
	}
	// Deep independence: mutating nested in clone must not affect src.
	if nested, ok := clone["nested"].(map[string]any); ok {
		nested["b"] = 999
		if orig, ok := src["nested"].(map[string]any); ok && orig["b"] == 999 {
			t.Fatalf("clone is not deep — nested map shared")
		}
	}
}

// TestCloneAnyMap covers the shallow-clone helper (empty -> nil, non-empty ->
// independent copy).
func TestCloneAnyMap(t *testing.T) {
	if got := cloneAnyMap(nil); got != nil {
		t.Fatalf("nil should be nil")
	}
	if got := cloneAnyMap(map[string]any{}); got != nil {
		t.Fatalf("empty should be nil")
	}
	src := map[string]any{"a": 1, "b": "x"}
	clone := cloneAnyMap(src)
	if clone["a"] != 1 || clone["b"] != "x" {
		t.Fatalf("shallow clone lost keys: %v", clone)
	}
	clone["a"] = 99
	if src["a"] != 1 {
		t.Fatalf("shallow clone should not share top-level keys with src")
	}
}

// TestHumanOptionsFromAny covers each type arm + non-map element skip.
func TestHumanOptionsFromAny(t *testing.T) {
	// Typed slice arm.
	got := humanOptionsFromAny([]humanrequest.HumanOption{{ID: "a", Label: "A"}})
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("typed slice: got %v", got)
	}
	// []map[string]any arm.
	got = humanOptionsFromAny([]map[string]any{{"id": " b ", "label": "B"}})
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("[]map arm: got %v", got)
	}
	// []any arm with a non-map element (skipped) + a valid one.
	got = humanOptionsFromAny([]any{"not-a-map", map[string]any{"id": "c", "label": "C"}})
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf("[]any arm: got %v", got)
	}
	// default arm.
	if humanOptionsFromAny("nope") != nil {
		t.Fatalf("default arm should be nil")
	}
}

// TestWaitingHumanSummary covers nil / question-from-request / reason fallback.
func TestWaitingHumanSummary(t *testing.T) {
	if got := waitingHumanSummary(nil); got != "" {
		t.Fatalf("nil interrupt should be empty")
	}
	// First pending human-request question wins.
	got := waitingHumanSummary(&RunInterrupt{
		HumanRequests: []humanrequest.HumanRequest{{Question: ""}, {Question: "  confirm?  "}},
		Reason:        "fallback",
	})
	if got != "confirm?" {
		t.Fatalf("question should win, got %q", got)
	}
	// No question -> reason fallback.
	got = waitingHumanSummary(&RunInterrupt{Reason: "  approval needed  "})
	if got != "approval needed" {
		t.Fatalf("reason fallback wrong, got %q", got)
	}
}
