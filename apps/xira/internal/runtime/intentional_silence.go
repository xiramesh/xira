package runtime

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
)

const (
	finishSilentToolName        = "finish_silent"
	finishSilentToolDescription = "Explicitly complete this Agent Turn without a public reply or owner notification. " +
		"Use only when you independently decide no outbound message is needed after any required work succeeds. " +
		"Do not call this after notify_owner succeeds; that successful notification already authorizes an empty public final. " +
		"This cannot hide a failed or rejected tool call. It accepts no arguments."
)

func finishSilentInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{},
		AdditionalProperties: rejectAllSchema(),
	}
}

func finishSilentToolCall(ctx context.Context, callID string, args map[string]any) ToolCallRecord {
	started := time.Now()
	callID = strings.TrimSpace(callID)
	if callID == "" {
		callID = uuid.NewString()
	}
	rec := ToolCallRecord{
		ID:        callID,
		Name:      finishSilentToolName,
		Input:     map[string]any{},
		StartedAt: started,
	}
	reject := func(err error) ToolCallRecord {
		rec.Error = err.Error()
		rec.Output = map[string]any{"status": "rejected", "error": err.Error()}
		rec.EndedAt = time.Now()
		return rec
	}
	if len(args) != 0 {
		return reject(errors.New("finish_silent does not accept arguments"))
	}
	if _, ok := runExecutionFromContext(ctx); !ok {
		return reject(errors.New("finish_silent requires runtime execution context"))
	}
	rec.Output = map[string]any{"status": "accepted"}
	rec.EndedAt = time.Now()
	return rec
}

func recordFinishSilentOutcome(
	rec ToolCallRecord,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) {
	status, _ := rec.Output["status"].(string)
	payload := map[string]any{
		"status":       status,
		"tool_call_id": rec.ID,
	}
	if rec.Error != "" {
		payload["error"] = rec.Error
	}
	recordEvent("agent.silence_declared", "runtime", "agent explicit silence "+status, payload)
	recordAudit(finishSilentToolName, finishSilentToolName, status == "accepted", "agent explicit silence "+status, map[string]any{
		"status":       status,
		"tool_call_id": rec.ID,
	})
}

// hasSuccessfulFinishSilent is strict: explicit silence is valid only when
// the declaration succeeded and no tool in the turn failed or was rejected.
// coverage: contract (100% required)
func hasSuccessfulFinishSilent(records []ToolCallRecord) bool {
	declared := false
	for _, record := range records {
		status, _ := record.Output["status"].(string)
		if record.Error != "" || status == "failed" || status == "rejected" {
			return false
		}
		if record.Name == finishSilentToolName && status == "accepted" {
			declared = true
		}
	}
	return declared
}

// intentionalSilenceReason resolves the sealed success reason shared by the
// ADK empty-final guard and Service verification. Explicit finish_silent uses
// strict all-tool success; notify_owner keeps its existing retry semantics.
// coverage: contract (100% required)
func intentionalSilenceReason(records []ToolCallRecord) (string, bool) {
	if hasSuccessfulFinishSilent(records) {
		return finishSilentToolName, true
	}
	if hasSuccessfulNotifyOwner(records) {
		return "notify_owner_sent", true
	}
	return "", false
}

// verifyRunOutcome keeps parent, child, and HITL-resume completion semantics
// identical. An empty final passes only through a sealed silence reason.
// coverage: contract (100% required)
func (s *Service) verifyRunOutcome(final string, records []ToolCallRecord, checks []string) VerificationResult {
	if strings.TrimSpace(final) == "" {
		if reason, ok := intentionalSilenceReason(records); ok {
			return VerificationResult{Status: "passed", Checks: []string{reason}}
		}
	}
	return s.verifier.Verify(final, checks)
}
