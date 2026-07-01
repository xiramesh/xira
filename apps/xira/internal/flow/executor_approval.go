package flow

import (
	"context"
	"fmt"
	"strings"
)

// doHumanApproval creates a HumanRequest for an explicit flow approval step
// and returns a waiting_human result. The full implementation lands in M5;
// the core create path is stable here so M4 executor tests can drive the
// agent path without a HumanRequestCreator.
func (e *AgentExecutor) doHumanApproval(ctx context.Context, run *Run, def *Definition, step Step) (StepExecutionResult, error) {
	if e == nil {
		return StepExecutionResult{Status: StepFailed, Error: "executor is required"}, nil
	}
	if e.Human == nil {
		return StepExecutionResult{Status: StepFailed, Error: "human request creator is not configured"}, nil
	}
	options := append([]string(nil), step.Executor.Options...)
	if len(options) == 0 {
		options = []string{"approve", "deny", "cancel"}
	}
	question := strings.TrimSpace(step.Executor.Question)
	if question == "" {
		question = strings.TrimSpace(step.Executor.Prompt)
	}
	if question == "" {
		question = "Approve step " + step.ID + "?"
	}
	metadata := buildFlowScopeMetadata(run, step)
	agentID := strings.TrimSpace(step.Executor.Agent)
	if agentID == "" {
		agentID = "flow:" + run.FlowID
	}
	sessionID := "flow:" + run.ID
	toolCallID := fmt.Sprintf("flow_human_approval:%s:%s", run.ID, step.ID)
	created, err := e.Human.CreateHumanRequest(ctx, CreateHumanRequestInput{
		WorkspaceID: e.Workspace,
		RunID:       run.ID,
		AgentID:     agentID,
		SessionID:   sessionID,
		ToolCallID:  toolCallID,
		Source:      SourceFlowHumanApproval,
		Kind:        "approval",
		Question:    question,
		Options:     options,
		DedupeKey:   toolCallID,
		Metadata:    metadata,
		Context:     runContext(run),
	})
	if err != nil {
		return StepExecutionResult{Status: StepFailed, Error: err.Error()}, nil
	}
	return StepExecutionResult{
		Status:          StepWaitingHuman,
		HumanRequestIDs: []string{created.ID},
		Interrupt: map[string]any{
			"reason":   SourceFlowHumanApproval,
			"source":   SourceFlowHumanApproval,
			"question": question,
			"options":  options,
		},
	}, nil
}

func buildFlowScopeMetadata(run *Run, step Step) map[string]string {
	return map[string]string{
		MetadataScopeType:  MetadataScopeTypeValue,
		MetadataFlowRunID:  run.ID,
		MetadataFlowStepID: step.ID,
		MetadataFlowID:     run.FlowID,
	}
}
