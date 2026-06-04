package runtime

import "strings"

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
