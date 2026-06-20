package runtime

import (
	"errors"
	"strings"
	"testing"
)

// TestNormalizeConfidence covers the canonical values + fallback.
func TestNormalizeConfidence(t *testing.T) {
	cases := map[string]string{
		"low":    "low",
		"medium": "medium",
		"high":   "high",
		// whitespace + case normalization
		"  HIGH ": "high",
		"Medium":  "medium",
		// fallback
		"extreme": "medium",
		"":        "medium",
		"  ":      "medium",
	}
	for in, want := range cases {
		if got := normalizeConfidence(in); got != want {
			t.Errorf("normalizeConfidence(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDelegateResultValidationError covers both Error branches + Unwrap.
func TestDelegateResultValidationError(t *testing.T) {
	// No underlying error.
	e := delegateResultValidationError{Reason: "why"}
	if got := e.Error(); got != "invalid_child_result" {
		t.Fatalf("nil-err Error = %q, want invalid_child_result", got)
	}
	// With underlying error: formatted string includes reason + inner.
	inner := errors.New("boom")
	e = delegateResultValidationError{Reason: "why", Err: inner}
	got := e.Error()
	if !strings.Contains(got, "invalid_child_result") || !strings.Contains(got, "why") || !strings.Contains(got, "boom") {
		t.Fatalf("Error() missing parts: %q", got)
	}
	if !errors.Is(e, inner) {
		t.Fatalf("Unwrap should expose inner error")
	}
}
