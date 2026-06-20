package runtime

import "strings"

// verifier is the contract Service depends on to verify a final response. It
// is an interface (not the concrete *VerificationRunner) so tests can inject a
// failing verifier without going through the network — this is what lets the
// failed-run path (non-empty final + failed verification) be exercised, which
// is exactly the assistant.final drain bug's trigger.
type verifier interface {
	Verify(finalResponse string, checks []string) VerificationResult
}

type VerificationRunner struct{}

func NewVerificationRunner() *VerificationRunner {
	return &VerificationRunner{}
}

func (r *VerificationRunner) Verify(finalResponse string, checks []string) VerificationResult {
	result := VerificationResult{Status: "passed", Checks: checks}
	for _, check := range checks {
		switch check {
		case "final_response_non_empty", "":
			if strings.TrimSpace(finalResponse) == "" {
				result.Status = "failed"
				result.Errors = append(result.Errors, "final response is empty")
			}
		default:
			result.Checks = append(result.Checks, "unknown:"+check)
		}
	}
	if len(checks) == 0 && strings.TrimSpace(finalResponse) == "" {
		result.Status = "failed"
		result.Errors = append(result.Errors, "final response is empty")
	}
	return result
}
