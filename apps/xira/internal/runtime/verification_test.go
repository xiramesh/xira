package runtime

import (
	"strings"
	"testing"
)

// TestVerifyPassesNonEmptyFinalWithNoChecks: a non-empty final with no checks
// configured passes (the len(checks)==0 && empty branch must NOT fire).
func TestVerifyPassesNonEmptyFinalWithNoChecks(t *testing.T) {
	r := NewVerificationRunner()
	res := r.Verify("hello", nil)
	if res.Status != "passed" {
		t.Fatalf("status = %q, want passed", res.Status)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("expected no errors, got %v", res.Errors)
	}
}

// TestVerifyFailsEmptyFinalWithNoChecks: the len(checks)==0 branch — an empty
// final with no explicit checks still fails with "final response is empty".
func TestVerifyFailsEmptyFinalWithNoChecks(t *testing.T) {
	r := NewVerificationRunner()
	res := r.Verify("", nil)
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed", res.Status)
	}
	if !containsError(res.Errors, "final response is empty") {
		t.Fatalf("expected empty-final error, got %v", res.Errors)
	}
}

// TestVerifyFailsWhitespaceFinalWithNoChecks: whitespace-only final is treated
// as empty.
func TestVerifyFailsWhitespaceFinalWithNoChecks(t *testing.T) {
	r := NewVerificationRunner()
	res := r.Verify("   \n\t  ", nil)
	if res.Status != "failed" {
		t.Fatalf("status = %q, want failed for whitespace-only final", res.Status)
	}
}

// TestVerifyExplicitNonEmptyCheck: the final_response_non_empty check passes on
// a populated final and fails on an empty one.
func TestVerifyExplicitNonEmptyCheck(t *testing.T) {
	r := NewVerificationRunner()
	if res := r.Verify("ok", []string{"final_response_non_empty"}); res.Status != "passed" {
		t.Fatalf("non-empty final with explicit check: status = %q, want passed", res.Status)
	}
	res := r.Verify("", []string{"final_response_non_empty"})
	if res.Status != "failed" {
		t.Fatalf("empty final with explicit check: status = %q, want failed", res.Status)
	}
	if !containsError(res.Errors, "final response is empty") {
		t.Fatalf("expected empty-final error, got %v", res.Errors)
	}
}

// TestVerifyEmptyStringCheckName: a check named "" behaves like
// final_response_non_empty (the `case "final_response_non_empty", ""` arm).
func TestVerifyEmptyStringCheckName(t *testing.T) {
	r := NewVerificationRunner()
	if res := r.Verify("ok", []string{""}); res.Status != "passed" {
		t.Fatalf(`check name "": status = %q, want passed`, res.Status)
	}
	if res := r.Verify("", []string{""}); res.Status != "failed" {
		t.Fatalf(`check name "" with empty final: status = %q, want failed`, res.Status)
	}
}

// TestVerifyUnknownCheckIsRecordedNotFailed: an unrecognized check name is
// appended to Checks as "unknown:<name>" but does not by itself fail the run.
func TestVerifyUnknownCheckIsRecordedNotFailed(t *testing.T) {
	r := NewVerificationRunner()
	res := r.Verify("ok", []string{"some_future_check"})
	if res.Status != "passed" {
		t.Fatalf("unknown check should not fail: status = %q", res.Status)
	}
	found := false
	for _, c := range res.Checks {
		if c == "unknown:some_future_check" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown check not recorded in Checks: %v", res.Checks)
	}
}

// TestVerifyPreservesConfiguredChecks: the original check names are kept in the
// result Checks (alongside any unknown: entries).
func TestVerifyPreservesConfiguredChecks(t *testing.T) {
	r := NewVerificationRunner()
	res := r.Verify("ok", []string{"final_response_non_empty", "weird"})
	hasNonEmpty := false
	for _, c := range res.Checks {
		if c == "final_response_non_empty" {
			hasNonEmpty = true
		}
	}
	if !hasNonEmpty {
		t.Fatalf("configured check not preserved: %v", res.Checks)
	}
}

func containsError(errs []string, want string) bool {
	for _, e := range errs {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}
