package runtime

import (
	"fmt"
	"time"
)

type EvolutionEngine struct{}

func NewEvolutionEngine() *EvolutionEngine {
	return &EvolutionEngine{}
}

func (e *EvolutionEngine) CandidateForFailure(runID, trigger string, verification VerificationResult, runErr error, now time.Time) *EvolutionCandidate {
	if verification.Status == "passed" && runErr == nil {
		return nil
	}
	evidence := append([]string(nil), verification.Errors...)
	failureLayer := "Verification"
	if runErr != nil {
		evidence = append(evidence, runErr.Error())
		failureLayer = "Model"
	}
	return &EvolutionCandidate{
		ID:           fmt.Sprintf("EV-%s", now.Format("20060102-150405")),
		RunID:        runID,
		Trigger:      trigger,
		FailureLayer: failureLayer,
		Evidence:     evidence,
		Status:       "candidate",
		CreatedAt:    now,
	}
}
