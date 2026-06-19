package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

func TestRealDeepSeekHITLHumanRequestTool(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Live HITL smoke: call human.request exactly once and ask `Approve shipping HITL v0 smoke test?`. Do not answer normally.",
		Context: channel.NewInboundContext("test", "live-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || len(resp.HumanRequests) != 1 {
		t.Fatalf("live human.request response status=%q human_requests=%d final=%q", resp.Status, len(resp.HumanRequests), resp.FinalResponse)
	}
	if resp.HumanRequests[0].Question == "" || resp.HumanRequests[0].Status == "" {
		t.Fatalf("live human request = %+v", resp.HumanRequests[0])
	}
}

func TestRealDeepSeekHITLRequireConfirmationSnapshot(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, true)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Live HITL smoke: call write_file exactly once with path `hitl-live-smoke.txt` and content `hitl-live-smoke`. Do not answer normally.",
		Context: channel.NewInboundContext("test", "live-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || len(resp.HumanRequests) != 1 {
		t.Fatalf("live confirmation response status=%q human_requests=%d final=%q tool_calls=%+v", resp.Status, len(resp.HumanRequests), resp.FinalResponse, resp.ToolCalls)
	}
	req := resp.HumanRequests[0]
	if req.ActionSnapshot == nil || req.ActionSnapshot.ToolName != "write_file" {
		t.Fatalf("live action snapshot = %+v", req.ActionSnapshot)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), req.ID, humanrequestApprove("live-smoke")); err != nil {
		t.Fatalf("approve live snapshot replay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rt.workspace, "hitl-live-smoke.txt")); err != nil {
		t.Fatalf("live approved write_file target missing: %v", err)
	}
}

func TestRealDeepSeekHITLRespondsAfterApprovedToolOutput(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, true)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Live HITL smoke: call write_file exactly once with path `hitl-live-final.txt` and content `hitl-live-final`, then wait for approval.",
		Context: channel.NewInboundContext("test", "live-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || len(resp.HumanRequests) != 1 || resp.HumanRequests[0].ActionSnapshot == nil {
		t.Fatalf("live confirmation response status=%q human_requests=%d final=%q", resp.Status, len(resp.HumanRequests), resp.FinalResponse)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), resp.HumanRequests[0].ID, humanrequestApprove("live-smoke")); err != nil {
		t.Fatalf("approve live snapshot replay: %v", err)
	}
	resumed, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "completed" || strings.TrimSpace(resumed.FinalResponse) == "" {
		t.Fatalf("live run after approved tool output status=%q final=%q tool_calls=%+v", resumed.Status, resumed.FinalResponse, resumed.ToolCalls)
	}
}

func TestRealDeepSeekHITLDelegateCompleted(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Live HITL smoke: delegate once to research-assistant. Child task: return a valid delegate_result_v1 JSON summary saying `delegate completed smoke`, with empty evidence_refs and confidence high. Do not ask a human.",
		Context: channel.NewInboundContext("test", "live-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != "completed" {
		t.Fatalf("live delegate completed status=%q final=%q tool_calls=%+v", resp.Status, resp.FinalResponse, resp.ToolCalls)
	}
	var sawDelegate bool
	for _, rec := range resp.ToolCalls {
		if rec.Name == delegateAgentToolName && anyString(rec.Output["status"]) == "completed" {
			sawDelegate = true
		}
	}
	if !sawDelegate {
		t.Fatalf("live delegate completed missing completed delegate output: %+v", resp.ToolCalls)
	}
}

func TestRealDeepSeekHITLDelegateChildWaiting(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Live HITL smoke: delegate once to research-assistant. The child must call human.request exactly once asking `Approve child HITL smoke?`.",
		Context: channel.NewInboundContext("test", "live-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || resp.Interrupt == nil || len(resp.Interrupt.DelegationJoinIDs) != 1 || len(resp.HumanRequests) != 1 {
		t.Fatalf("live delegate response status=%q joins=%+v human_requests=%d final=%q", resp.Status, resp.Interrupt, len(resp.HumanRequests), resp.FinalResponse)
	}
}

func TestRealDeepSeekFlowAgentStepCompletes(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)
	flowPath := filepath.Join(t.TempDir(), "flow.yaml")
	writeFile(t, flowPath, `schema_version: xira.flow.v0
id: live-deepseek-flow-smoke
name: Live DeepSeek Flow Smoke
version: 0.1.0
objective: verify a real DeepSeek-backed flow agent step
entrypoints:
  - id: ad_hoc
    start_step: summarize
    required_inputs:
      - request
steps:
  - id: summarize
    objective: Return a concise smoke-test summary for the request.
    instructions:
      - Include the exact phrase "live flow smoke".
      - Keep the response under 40 words.
    executor:
      agent: xira-assistant
    output_contract:
      required_slots:
        - id: summary
`)
	started, err := rt.StartFlow(context.Background(), FlowStartRequest{
		FlowPath:     flowPath,
		EntrypointID: "ad_hoc",
		Input:        map[string]string{"request": "confirm Flow can call DeepSeek end to end"},
	})
	if err != nil {
		t.Fatalf("StartFlow() error = %v", err)
	}
	if started.Status != "running" || started.CurrentStepID != "summarize" {
		t.Fatalf("started status=%q current=%q", started.Status, started.CurrentStepID)
	}
	advanced, err := rt.AdvanceFlow(context.Background(), started.ID)
	if err != nil {
		t.Fatalf("AdvanceFlow() error = %v", err)
	}
	step := advanced.Steps["summarize"]
	if advanced.Status != "completed" || step.Status != "completed" {
		t.Fatalf("advanced status=%q step=%+v", advanced.Status, step)
	}
	if strings.TrimSpace(step.AgentRunID) == "" {
		t.Fatalf("missing agent_run_id in completed live flow step: %+v", step)
	}
	if out := step.Outputs["summary"]; strings.TrimSpace(out.Summary) == "" && out.Value == nil {
		t.Fatalf("missing summary output in completed live flow step: %+v", step.Outputs)
	}

	// Session history must persist to a messages.jsonl under the trigger
	// channel tree (cli, since this flow run omitted Context), NOT under a
	// forged "flow" channel tree. Live proof that flow propagates trigger
	// identity end to end.
	agentRun, err := rt.RunStore().Load(step.AgentRunID)
	if err != nil {
		t.Fatalf("load agent run for live flow step: %v", err)
	}
	if agentRun.SessionScope == nil {
		t.Fatal("live flow agent run session scope is nil")
	}
	if agentRun.SessionScope.Channel == "flow" {
		t.Fatalf("live flow agent run forged channel \"flow\"; session scope = %+v", agentRun.SessionScope)
	}
	if agentRun.SessionScope.Channel == "" {
		t.Fatalf("live flow agent run session channel empty; session scope = %+v", agentRun.SessionScope)
	}
	// Find the persisted messages.jsonl under the session root and assert it
	// lives under the trigger channel, not a forged flow tree.
	sessionRoot := filepath.Join(rt.StateRoot(), "sessions")
	wantChannel := agentRun.SessionScope.Channel
	var msgPath string
	_ = filepath.Walk(sessionRoot, func(path string, _ os.FileInfo, _ error) error {
		if strings.HasSuffix(path, "messages.jsonl") && strings.Contains(path, agentRun.AgentID) {
			msgPath = path
		}
		return nil
	})
	if msgPath == "" {
		t.Fatalf("no messages.jsonl persisted under %s for agent %s", sessionRoot, agentRun.AgentID)
	}
	rel := strings.TrimPrefix(filepath.ToSlash(msgPath), filepath.ToSlash(sessionRoot)+"/")
	if !strings.HasPrefix(rel, wantChannel+"/") {
		t.Fatalf("live flow session persisted under %q, want channel %q: %s", rel, wantChannel, msgPath)
	}
	if strings.HasPrefix(rel, "flow/") {
		t.Fatalf("live flow session persisted under forged sessions/flow/ tree: %s", msgPath)
	}
}

func TestRealDeepSeekFlowRoutesToHumanApproval(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)
	flowPath := filepath.Join(t.TempDir(), "flow.yaml")
	writeFile(t, flowPath, `schema_version: xira.flow.v0
id: live-deepseek-flow-hitl-smoke
name: Live DeepSeek Flow HITL Smoke
version: 0.1.0
objective: verify a real DeepSeek-backed flow can pause for human approval
entrypoints:
  - id: ad_hoc
    start_step: design
    required_inputs:
      - request
steps:
  - id: design
    objective: Produce a short implementation design for the request.
    instructions:
      - Return a concise answer under 60 words.
      - Include the exact phrase "live flow hitl".
    executor:
      agent: xira-assistant
      tools: []
    output_contract:
      required_slots:
        - id: design_doc
    transitions:
      branches:
        - when: "${runtime.policy.require_design_approval == true}"
          next: approve_design
        - when: "${runtime.policy.require_design_approval != true}"
          next: report
  - id: approve_design
    objective: Human approval gate for the live DeepSeek flow design.
    executor:
      type: human_approval
      question: "Approve the live DeepSeek Flow HITL smoke design?"
      options:
        - approve
        - reject
    output_contract:
      required_slots:
        - id: approval_signal
    transitions:
      branches:
        - when: "${outputs.approve_design.approval_signal == 'approve'}"
          next: report
        - when: "${outputs.approve_design.approval_signal == 'reject'}"
          next: report
  - id: report
    objective: Report final result.
    executor:
      agent: xira-assistant
    output_contract:
      required_slots:
        - id: final_report
`)
	started, err := rt.StartFlow(context.Background(), FlowStartRequest{
		FlowPath:     flowPath,
		EntrypointID: "ad_hoc",
		Input: map[string]string{
			"request":                 "confirm Flow with real DeepSeek can pause for HITL",
			"require_design_approval": "true",
		},
	})
	if err != nil {
		t.Fatalf("StartFlow() error = %v", err)
	}
	afterDesign, err := rt.AdvanceFlow(context.Background(), started.ID)
	if err != nil {
		t.Fatalf("AdvanceFlow design error = %v", err)
	}
	designStep := afterDesign.Steps["design"]
	if designStep.Status != "completed" || strings.TrimSpace(designStep.AgentRunID) == "" {
		t.Fatalf("design step not completed by live DeepSeek: status=%q agent_run_id=%q error=%q", designStep.Status, designStep.AgentRunID, designStep.Error)
	}
	if afterDesign.Status != "running" || afterDesign.CurrentStepID != "approve_design" {
		t.Fatalf("after design status=%q current=%q, want running approve_design", afterDesign.Status, afterDesign.CurrentStepID)
	}

	waiting, err := rt.AdvanceFlow(context.Background(), started.ID)
	if err != nil {
		t.Fatalf("AdvanceFlow approval error = %v", err)
	}
	approvalStep := waiting.Steps["approve_design"]
	if waiting.Status != StatusWaitingHuman || approvalStep.Status != StatusWaitingHuman || len(approvalStep.HumanRequestIDs) != 1 {
		t.Fatalf("flow did not pause for human approval: status=%q step=%+v", waiting.Status, approvalStep)
	}
	req, err := rt.GetHumanRequest(context.Background(), approvalStep.HumanRequestIDs[0])
	if err != nil {
		t.Fatalf("GetHumanRequest() error = %v", err)
	}
	if req.Status != humanrequest.StatusPending || req.Source != "flow_human_approval" || req.ToolCallID == "" {
		t.Fatalf("unexpected live flow human request: status=%q source=%q tool_call_id=%q", req.Status, req.Source, req.ToolCallID)
	}
}

func TestRealDeepSeekLongFlowFourAgentsWithHITL(t *testing.T) {
	rt := newLiveDeepSeekLongFlowService(t)
	flowPath := filepath.Join(t.TempDir(), "long-flow.yaml")
	writeFile(t, flowPath, `schema_version: xira.flow.v0
id: live-deepseek-long-flow-hitl
name: Live DeepSeek Long Flow HITL
version: 0.1.0
objective: verify a 10-step multi-agent flow with a human approval gate
entrypoints:
  - id: ad_hoc
    start_step: triage
    required_inputs:
      - request
steps:
  - id: triage
    objective: Summarize the request into a compact execution brief.
    instructions:
      - Return under 25 words.
      - Include "step triage".
    executor:
      agent: flow-intake
      tools:
        - read_file
      tool_input_allowlist:
        read_file:
          path:
            - case/request.md
    output_contract:
      required_slots:
        - id: triage_brief
    transitions:
      on_success: context_research
  - id: context_research
    objective: Identify two context facts for the brief.
    instructions:
      - Return under 30 words.
      - Include "step context".
    executor:
      agent: flow-research
      tools:
        - list_dir
      tool_input_allowlist:
        list_dir:
          path:
            - case
    inputs:
      triage_brief: "${outputs.triage.triage_brief}"
    output_contract:
      required_slots:
        - id: context_notes
    transitions:
      on_success: constraint_research
  - id: constraint_research
    objective: Identify two constraints that should shape the plan.
    instructions:
      - Return under 30 words.
      - Include "step constraints".
    executor:
      agent: flow-research
      tools:
        - search_file
      tool_input_allowlist:
        search_file:
          root:
            - case
          query:
            - BUDGET-MARKER-284
    inputs:
      context_notes: "${outputs.context_research.context_notes}"
    output_contract:
      required_slots:
        - id: constraints
    transitions:
      on_success: architecture_plan
  - id: architecture_plan
    objective: Draft a small architecture plan.
    instructions:
      - Return under 35 words.
      - Include "step architecture".
    executor:
      agent: flow-architect
      tools:
        - read_file
      tool_input_allowlist:
        read_file:
          path:
            - case/architecture.md
    inputs:
      constraints: "${outputs.constraint_research.constraints}"
    output_contract:
      required_slots:
        - id: architecture_plan
    transitions:
      on_success: risk_review
  - id: risk_review
    objective: Review the plan for one risk and one mitigation.
    instructions:
      - Return under 35 words.
      - Include "step risk".
    executor:
      agent: flow-reviewer
      tools:
        - search_file
      tool_input_allowlist:
        search_file:
          root:
            - case
          query:
            - RISK-MARKER-903
    inputs:
      architecture_plan: "${outputs.architecture_plan.architecture_plan}"
    output_contract:
      required_slots:
        - id: risk_review
    transitions:
      on_success: approve_plan
  - id: approve_plan
    objective: Human approval gate for the live long flow plan.
    executor:
      type: human_approval
      question: "Approve the live 10-step DeepSeek long flow plan?"
      options:
        - approve
        - reject
    output_contract:
      required_slots:
        - id: approval_signal
    transitions:
      branches:
        - when: "${outputs.approve_plan.approval_signal == 'approve'}"
          next: implementation_slice
        - when: "${outputs.approve_plan.approval_signal == 'reject'}"
          next: final_report
  - id: implementation_slice
    objective: Propose a minimal implementation slice.
    instructions:
      - Return under 35 words.
      - Include "step implementation".
    executor:
      agent: flow-architect
      tools:
        - read_file
      tool_input_allowlist:
        read_file:
          path:
            - case/implementation.md
    inputs:
      approval_signal: "${outputs.approve_plan.approval_signal}"
    output_contract:
      required_slots:
        - id: implementation_slice
    transitions:
      on_success: test_plan
  - id: test_plan
    objective: Define a compact test plan.
    instructions:
      - Return under 35 words.
      - Include "step tests".
    executor:
      agent: flow-reviewer
      tools:
        - search_file
      tool_input_allowlist:
        search_file:
          root:
            - case
          query:
            - TEST-MARKER-626
    inputs:
      implementation_slice: "${outputs.implementation_slice.implementation_slice}"
    output_contract:
      required_slots:
        - id: test_plan
    transitions:
      on_success: release_notes
  - id: release_notes
    objective: Write short release notes for the completed work.
    instructions:
      - Return under 35 words.
      - Include "step release".
    executor:
      agent: flow-intake
      tools:
        - read_file
      tool_input_allowlist:
        read_file:
          path:
            - case/release.md
    inputs:
      test_plan: "${outputs.test_plan.test_plan}"
    output_contract:
      required_slots:
        - id: release_notes
    transitions:
      on_success: final_report
  - id: final_report
    objective: Produce a final concise report.
    instructions:
      - Return under 40 words.
      - Include "step final".
    executor:
      agent: flow-research
      tools:
        - list_dir
      tool_input_allowlist:
        list_dir:
          path:
            - case
    inputs:
      release_notes: "${outputs.release_notes.release_notes || 'rejected before implementation'}"
    output_contract:
      required_slots:
        - id: final_report
`)
	run, err := rt.StartFlow(context.Background(), FlowStartRequest{
		FlowPath:     flowPath,
		EntrypointID: "ad_hoc",
		Input: map[string]string{
			"request": "Run a live 10-step multi-agent Flow smoke with HITL and concise outputs.",
		},
	})
	if err != nil {
		t.Fatalf("StartFlow() error = %v", err)
	}

	for i := 0; i < 8 && run.Status != StatusWaitingHuman; i++ {
		run, err = rt.AdvanceFlow(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("AdvanceFlow before HITL step %d error = %v", i+1, err)
		}
	}
	if run.Status != StatusWaitingHuman || run.CurrentStepID != "approve_plan" {
		t.Fatalf("flow did not stop at approve_plan: status=%q current=%q steps=%+v", run.Status, run.CurrentStepID, run.Steps)
	}
	approvalIDs := run.Steps["approve_plan"].HumanRequestIDs
	if len(approvalIDs) != 1 {
		t.Fatalf("approve_plan human_request_ids = %+v, want one", approvalIDs)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), approvalIDs[0], humanrequestApprove("live-long-flow")); err != nil {
		t.Fatalf("ResolveHumanRequest approve error = %v", err)
	}
	run, err = rt.ResumeFlow(context.Background(), run.ID, approvalIDs[0])
	if err != nil {
		t.Fatalf("ResumeFlow after approval error = %v", err)
	}

	for i := 0; i < 8 && run.Status != "completed"; i++ {
		run, err = rt.AdvanceFlow(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("AdvanceFlow after HITL step %d error = %v", i+1, err)
		}
	}
	if run.Status != "completed" {
		t.Fatalf("long flow status=%q current=%q, want completed", run.Status, run.CurrentStepID)
	}
	stepIDs := []string{"triage", "context_research", "constraint_research", "architecture_plan", "risk_review", "approve_plan", "implementation_slice", "test_plan", "release_notes", "final_report"}
	for _, stepID := range stepIDs {
		if got := run.Steps[stepID].Status; got != "completed" {
			t.Fatalf("step %s status=%q, want completed", stepID, got)
		}
	}
	agentsSeen := map[string]bool{}
	for _, stepID := range []string{"triage", "context_research", "constraint_research", "architecture_plan", "risk_review", "implementation_slice", "test_plan", "release_notes", "final_report"} {
		agentRunID := run.Steps[stepID].AgentRunID
		if strings.TrimSpace(agentRunID) == "" {
			t.Fatalf("step %s missing agent_run_id", stepID)
		}
		agentRun, err := rt.RunStore().Load(agentRunID)
		if err != nil {
			t.Fatalf("load agent run for step %s: %v", stepID, err)
		}
		agentsSeen[agentRun.AgentID] = true
	}
	for _, agentID := range []string{"flow-intake", "flow-research", "flow-architect", "flow-reviewer"} {
		if !agentsSeen[agentID] {
			t.Fatalf("agent %s was not used; seen=%v", agentID, agentsSeen)
		}
	}
}

func TestRealDeepSeekLongFlowFourAgentsWithToolsAndHITL(t *testing.T) {
	rt := newLiveDeepSeekLongFlowToolService(t)
	flowPath := filepath.Join(t.TempDir(), "long-flow-tools.yaml")
	writeFile(t, flowPath, `schema_version: xira.flow.v0
id: live-deepseek-long-flow-tools-hitl
name: Live DeepSeek Long Flow Tools HITL
version: 0.1.0
objective: verify a 10-step multi-agent flow that uses workspace tools and a human approval gate
entrypoints:
  - id: ad_hoc
    start_step: triage
    required_inputs:
      - request
steps:
  - id: triage
    objective: Read the request seed and summarize it.
    instructions:
      - You MUST call read_file with path "case/request.md" before answering.
      - Include "step triage" and "REQ-MARKER-741".
      - Return under 35 words.
    executor:
      agent: flow-intake
    output_contract:
      required_slots:
        - id: triage_brief
    transitions:
      on_success: context_research
  - id: context_research
    objective: Inspect the case directory before writing context notes.
    instructions:
      - You MUST call list_dir with path "case" before answering.
      - Include "step context" and mention at least two listed filenames.
      - Return under 40 words.
    executor:
      agent: flow-research
    inputs:
      triage_brief: "${outputs.triage.triage_brief}"
    output_contract:
      required_slots:
        - id: context_notes
    transitions:
      on_success: constraint_research
  - id: constraint_research
    objective: Search for budget constraints.
    instructions:
      - You MUST call search_file with query "BUDGET-MARKER-284" and root "case" before answering.
      - Include "step constraints" and "BUDGET-MARKER-284".
      - Return under 40 words.
    executor:
      agent: flow-research
    inputs:
      context_notes: "${outputs.context_research.context_notes}"
    output_contract:
      required_slots:
        - id: constraints
    transitions:
      on_success: architecture_plan
  - id: architecture_plan
    objective: Read architecture facts and draft the plan.
    instructions:
      - You MUST call read_file with path "case/architecture.md" before answering.
      - Include "step architecture" and "ARCH-MARKER-519".
      - Return under 45 words.
    executor:
      agent: flow-architect
    inputs:
      constraints: "${outputs.constraint_research.constraints}"
    output_contract:
      required_slots:
        - id: architecture_plan
    transitions:
      on_success: risk_review
  - id: risk_review
    objective: Search for a known risk and mitigation.
    instructions:
      - You MUST call search_file with query "RISK-MARKER-903" and root "case" before answering.
      - Include "step risk" and "RISK-MARKER-903".
      - Return under 45 words.
    executor:
      agent: flow-reviewer
    inputs:
      architecture_plan: "${outputs.architecture_plan.architecture_plan}"
    output_contract:
      required_slots:
        - id: risk_review
    transitions:
      on_success: approve_plan
  - id: approve_plan
    objective: Human approval gate for the live tool-using long flow plan.
    executor:
      type: human_approval
      question: "Approve the live 10-step DeepSeek tool-using long flow plan?"
      options:
        - approve
        - reject
    output_contract:
      required_slots:
        - id: approval_signal
    transitions:
      branches:
        - when: "${outputs.approve_plan.approval_signal == 'approve'}"
          next: implementation_slice
        - when: "${outputs.approve_plan.approval_signal == 'reject'}"
          next: final_report
  - id: implementation_slice
    objective: Read implementation guidance and propose the slice.
    instructions:
      - You MUST call read_file with path "case/implementation.md" before answering.
      - Include "step implementation" and "IMPL-MARKER-337".
      - Return under 45 words.
    executor:
      agent: flow-architect
    inputs:
      approval_signal: "${outputs.approve_plan.approval_signal}"
    output_contract:
      required_slots:
        - id: implementation_slice
    transitions:
      on_success: test_plan
  - id: test_plan
    objective: Search for test obligations.
    instructions:
      - You MUST call search_file with query "TEST-MARKER-626" and root "case" before answering.
      - Include "step tests" and "TEST-MARKER-626".
      - Return under 45 words.
    executor:
      agent: flow-reviewer
    inputs:
      implementation_slice: "${outputs.implementation_slice.implementation_slice}"
    output_contract:
      required_slots:
        - id: test_plan
    transitions:
      on_success: release_notes
  - id: release_notes
    objective: Read release note guidance.
    instructions:
      - You MUST call read_file with path "case/release.md" before answering.
      - Include "step release" and "REL-MARKER-812".
      - Return under 45 words.
    executor:
      agent: flow-intake
    inputs:
      test_plan: "${outputs.test_plan.test_plan}"
    output_contract:
      required_slots:
        - id: release_notes
    transitions:
      on_success: final_report
  - id: final_report
    objective: Inspect the case directory again and produce the final report.
    instructions:
      - You MUST call list_dir with path "case" before answering.
      - Include "step final" and "FLOW-TOOLS-DONE".
      - Do not mention HITL checkpoints other than approve_plan.
      - Return under 50 words.
    executor:
      agent: flow-research
    inputs:
      release_notes: "${outputs.release_notes.release_notes || 'rejected before implementation'}"
    output_contract:
      required_slots:
        - id: final_report
`)
	run, err := rt.StartFlow(context.Background(), FlowStartRequest{
		FlowPath:     flowPath,
		EntrypointID: "ad_hoc",
		Input: map[string]string{
			"request": "Run a live 10-step multi-agent Flow with real workspace tool use and HITL.",
		},
	})
	if err != nil {
		t.Fatalf("StartFlow() error = %v", err)
	}

	for i := 0; i < 8 && run.Status != StatusWaitingHuman; i++ {
		run, err = rt.AdvanceFlow(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("AdvanceFlow before tool HITL step %d error = %v", i+1, err)
		}
	}
	if run.Status != StatusWaitingHuman || run.CurrentStepID != "approve_plan" {
		t.Fatalf("tool flow did not stop at approve_plan: status=%q current=%q steps=%+v", run.Status, run.CurrentStepID, run.Steps)
	}
	approvalIDs := run.Steps["approve_plan"].HumanRequestIDs
	if len(approvalIDs) != 1 {
		t.Fatalf("approve_plan human_request_ids = %+v, want one", approvalIDs)
	}
	if _, err := rt.ResolveHumanRequest(context.Background(), approvalIDs[0], humanrequestApprove("live-long-flow-tools")); err != nil {
		t.Fatalf("ResolveHumanRequest approve error = %v", err)
	}
	run, err = rt.ResumeFlow(context.Background(), run.ID, approvalIDs[0])
	if err != nil {
		t.Fatalf("ResumeFlow after approval error = %v", err)
	}

	for i := 0; i < 8 && run.Status != "completed"; i++ {
		run, err = rt.AdvanceFlow(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("AdvanceFlow after tool HITL step %d error = %v", i+1, err)
		}
	}
	if run.Status != "completed" {
		t.Fatalf("tool long flow status=%q current=%q, want completed", run.Status, run.CurrentStepID)
	}
	stepIDs := []string{"triage", "context_research", "constraint_research", "architecture_plan", "risk_review", "approve_plan", "implementation_slice", "test_plan", "release_notes", "final_report"}
	for _, stepID := range stepIDs {
		if got := run.Steps[stepID].Status; got != "completed" {
			t.Fatalf("step %s status=%q, want completed", stepID, got)
		}
	}

	agentsSeen := map[string]bool{}
	toolCounts := map[string]int{}
	totalToolCalls := 0
	for _, stepID := range []string{"triage", "context_research", "constraint_research", "architecture_plan", "risk_review", "implementation_slice", "test_plan", "release_notes", "final_report"} {
		agentRunID := run.Steps[stepID].AgentRunID
		if strings.TrimSpace(agentRunID) == "" {
			t.Fatalf("step %s missing agent_run_id", stepID)
		}
		agentRun, err := rt.RunStore().Load(agentRunID)
		if err != nil {
			t.Fatalf("load agent run for step %s: %v", stepID, err)
		}
		agentsSeen[agentRun.AgentID] = true
		if len(agentRun.ToolCalls) == 0 {
			t.Fatalf("step %s agent run %s recorded no tool calls", stepID, agentRunID)
		}
		for _, call := range agentRun.ToolCalls {
			if call.Error != "" {
				t.Fatalf("step %s tool %s failed: %s input=%+v", stepID, call.Name, call.Error, call.Input)
			}
			toolCounts[call.Name]++
			totalToolCalls++
		}
		assertPersistedToolCalls(t, rt, agentRunID, len(agentRun.ToolCalls))
	}
	for _, agentID := range []string{"flow-intake", "flow-research", "flow-architect", "flow-reviewer"} {
		if !agentsSeen[agentID] {
			t.Fatalf("agent %s was not used; seen=%v", agentID, agentsSeen)
		}
	}
	for _, toolName := range []string{"read_file", "list_dir", "search_file"} {
		if toolCounts[toolName] == 0 {
			t.Fatalf("tool %s was not used; counts=%v total=%d", toolName, toolCounts, totalToolCalls)
		}
	}
	if totalToolCalls < 9 {
		t.Fatalf("total tool calls = %d, want at least one per agent step; counts=%v", totalToolCalls, toolCounts)
	}
}

func TestRealDeepSeekFlowFileArtifactsSkipReadWithSkill(t *testing.T) {
	rt := newLiveDeepSeekFlowFileSkillService(t)
	flowPath := filepath.Join(t.TempDir(), "file-artifacts-flow.yaml")
	writeFile(t, flowPath, `schema_version: xira.flow.v0
id: live-deepseek-flow-file-artifacts-skill
name: Live DeepSeek Flow File Artifacts Skill
version: 0.1.0
objective: verify a file-backed multi-agent flow with skip-step reads and an activated skill
entrypoints:
  - id: ad_hoc
    start_step: write_brief
    required_inputs:
      - request
steps:
  - id: write_brief
    objective: Create the initial brief file.
    instructions:
      - Use the flow-file-artifact skill.
      - Do not call read_file, search_file, or list_dir in this step.
      - You MUST call write_file exactly once with path "artifacts/flow-files/01-brief.md".
      - The file content MUST include "BRIEF-MARKER-101" and "request baseline".
      - After the approved write, answer with a short confirmation only.
    executor:
      agent: flow-writer
      tools:
        - write_file
      tool_input_allowlist:
        write_file:
          path:
            - artifacts/flow-files/01-brief.md
    transitions:
      on_success: write_research
  - id: write_research
    objective: Create the research file.
    instructions:
      - Use the flow-file-artifact skill.
      - Do not call read_file, search_file, or list_dir in this step.
      - You MUST call write_file exactly once with path "artifacts/flow-files/02-research.md".
      - The file content MUST include "RESEARCH-MARKER-202" and "source evidence".
      - After the approved write, answer with a short confirmation only.
    executor:
      agent: flow-research
      tools:
        - write_file
      tool_input_allowlist:
        write_file:
          path:
            - artifacts/flow-files/02-research.md
    transitions:
      on_success: write_constraints
  - id: write_constraints
    objective: Create the constraints file.
    instructions:
      - Use the flow-file-artifact skill.
      - Do not call read_file, search_file, or list_dir in this step.
      - You MUST call write_file exactly once with path "artifacts/flow-files/03-constraints.md".
      - The file content MUST include "CONSTRAINT-MARKER-303" and "approval required".
      - After the approved write, answer with a short confirmation only.
    executor:
      agent: flow-reviewer
      tools:
        - write_file
      tool_input_allowlist:
        write_file:
          path:
            - artifacts/flow-files/03-constraints.md
    transitions:
      on_success: synthesize_plan
  - id: synthesize_plan
    objective: Read non-adjacent earlier files and create the plan file.
    instructions:
      - Use the flow-file-artifact skill.
      - You MUST call read_file for "artifacts/flow-files/01-brief.md".
      - You MUST call read_file for "artifacts/flow-files/03-constraints.md".
      - Then call write_file exactly once with path "artifacts/flow-files/04-plan.md".
      - The file content MUST include "PLAN-MARKER-404", "BRIEF-MARKER-101", and "CONSTRAINT-MARKER-303".
      - "The plan file MUST describe only the actual Flow steps in this definition: write_brief, write_research, write_constraints, synthesize_plan, approve_plan, implementation_slice, risk_review, test_plan, release_notes, final_report."
      - The plan MUST state that write_brief, write_research, and write_constraints do not call read_file, search_file, or list_dir.
      - The plan MUST state that write_research does not read 01-brief.md, and write_constraints does not read 01-brief.md or 02-research.md.
      - The plan MUST describe dependencies from the actual Flow definition and tool contract only. Do not infer extra reads from artifact numbering.
      - 'The plan MUST include a section headed "## Tool Contract Table" with these exact rows:'
      - '| write_brief | no read/search/list | 01-brief.md |'
      - '| write_research | no read/search/list | 02-research.md |'
      - '| write_constraints | no read/search/list | 03-constraints.md |'
      - '| synthesize_plan | read 01-brief.md and 03-constraints.md | 04-plan.md |'
      - '| implementation_slice | read 04-plan.md | 05-implementation.md |'
      - '| risk_review | read 02-research.md and 05-implementation.md | 06-risk.md |'
      - '| test_plan | search BRIEF-MARKER-101 under artifacts/flow-files; read 06-risk.md | 07-test-plan.md |'
      - '| release_notes | read 03-constraints.md and 07-test-plan.md | 08-release.md |'
      - '| final_report | list artifacts/flow-files; read 01-brief.md, 04-plan.md, and 08-release.md | 09-final-report.md |'
      - The only artifact filenames the plan may mention are 01-brief.md, 02-research.md, 03-constraints.md, 04-plan.md, 05-implementation.md, 06-risk.md, 07-test-plan.md, 08-release.md, and 09-final-report.md.
      - Do not invent skill files, review files, shortened names, or alternate artifact names.
      - After the approved write, answer with a short confirmation only.
    executor:
      agent: flow-architect
      tools:
        - read_file
        - write_file
      tool_input_allowlist:
        read_file:
          path:
            - artifacts/flow-files/01-brief.md
            - artifacts/flow-files/03-constraints.md
        write_file:
          path:
            - artifacts/flow-files/04-plan.md
    transitions:
      on_success: approve_plan
  - id: approve_plan
    objective: Human approval gate after the file-backed plan is written.
    executor:
      type: human_approval
      question: "Approve the file-backed plan in artifacts/flow-files/04-plan.md?"
      options:
        - approve
        - reject
    transitions:
      branches:
        - when: "${outputs.approve_plan.approval_signal == 'approve'}"
          next: implementation_slice
        - when: "${outputs.approve_plan.approval_signal == 'reject'}"
          next: final_report
    output_contract:
      required_slots:
        - id: approval_signal
  - id: implementation_slice
    objective: Read the approved plan and create the implementation file.
    instructions:
      - Use the flow-file-artifact skill.
      - You MUST call read_file for "artifacts/flow-files/04-plan.md".
      - Then call write_file exactly once with path "artifacts/flow-files/05-implementation.md".
      - The file content MUST include "IMPL-MARKER-505", "PLAN-MARKER-404", and "approved plan".
      - After the approved write, answer with a short confirmation only.
    executor:
      agent: flow-architect
      tools:
        - read_file
        - write_file
      tool_input_allowlist:
        read_file:
          path:
            - artifacts/flow-files/04-plan.md
        write_file:
          path:
            - artifacts/flow-files/05-implementation.md
    transitions:
      on_success: risk_review
  - id: risk_review
    objective: Jump back to research and combine it with implementation risk.
    instructions:
      - Use the flow-file-artifact skill.
      - You MUST call read_file for "artifacts/flow-files/02-research.md".
      - You MUST call read_file for "artifacts/flow-files/05-implementation.md".
      - Then call write_file exactly once with path "artifacts/flow-files/06-risk.md".
      - The file content MUST include "RISK-MARKER-606", "RESEARCH-MARKER-202", and "IMPL-MARKER-505".
      - The risk file MUST discuss only risks in the produced Flow artifact chain. Do not mention non-artifact source files or any filename outside the allowed artifact filename set.
      - After the approved write, answer with a short confirmation only.
    executor:
      agent: flow-reviewer
      tools:
        - read_file
        - write_file
      tool_input_allowlist:
        read_file:
          path:
            - artifacts/flow-files/02-research.md
            - artifacts/flow-files/05-implementation.md
        write_file:
          path:
            - artifacts/flow-files/06-risk.md
    transitions:
      on_success: test_plan
  - id: test_plan
    objective: Search prior artifacts and create a test plan file.
    instructions:
      - Use the flow-file-artifact skill.
      - You MUST call search_file with query "BRIEF-MARKER-101" and root "artifacts/flow-files".
      - You MUST call read_file for "artifacts/flow-files/06-risk.md".
      - Then call write_file exactly once with path "artifacts/flow-files/07-test-plan.md".
      - The file content MUST include "TEST-MARKER-707", "BRIEF-MARKER-101", and "RISK-MARKER-606".
      - The test plan MUST validate only the files in artifacts/flow-files. Do not reference non-artifact source files or any filename outside the allowed artifact filename set.
      - After the approved write, answer with a short confirmation only.
    executor:
      agent: flow-reviewer
      tools:
        - search_file
        - read_file
        - write_file
      tool_input_allowlist:
        search_file:
          root:
            - artifacts/flow-files
          query:
            - BRIEF-MARKER-101
        read_file:
          path:
            - artifacts/flow-files/06-risk.md
        write_file:
          path:
            - artifacts/flow-files/07-test-plan.md
    transitions:
      on_success: release_notes
  - id: release_notes
    objective: Jump back to constraints and combine them with the test plan.
    instructions:
      - Use the flow-file-artifact skill.
      - You MUST call read_file for "artifacts/flow-files/03-constraints.md".
      - You MUST call read_file for "artifacts/flow-files/07-test-plan.md".
      - Then call write_file exactly once with path "artifacts/flow-files/08-release.md".
      - The file content MUST include "RELEASE-MARKER-808", "CONSTRAINT-MARKER-303", and "TEST-MARKER-707".
      - After the approved write, answer with a short confirmation only.
    executor:
      agent: flow-writer
      tools:
        - read_file
        - write_file
      tool_input_allowlist:
        read_file:
          path:
            - artifacts/flow-files/03-constraints.md
            - artifacts/flow-files/07-test-plan.md
        write_file:
          path:
            - artifacts/flow-files/08-release.md
    transitions:
      on_success: final_report
  - id: final_report
    objective: List all artifact files, read non-adjacent outputs, and write the final report.
    instructions:
      - Use the flow-file-artifact skill.
      - You MUST call list_dir with path "artifacts/flow-files".
      - You MUST call read_file for "artifacts/flow-files/01-brief.md".
      - You MUST call read_file for "artifacts/flow-files/04-plan.md".
      - You MUST call read_file for "artifacts/flow-files/08-release.md".
      - Then call write_file exactly once with path "artifacts/flow-files/09-final-report.md".
      - The file content MUST include "FINAL-MARKER-909", "BRIEF-MARKER-101", "PLAN-MARKER-404", and "RELEASE-MARKER-808".
      - After the approved write, answer with a short confirmation only.
    executor:
      agent: flow-research
      tools:
        - list_dir
        - read_file
        - write_file
      tool_input_allowlist:
        list_dir:
          path:
            - artifacts/flow-files
        read_file:
          path:
            - artifacts/flow-files/01-brief.md
            - artifacts/flow-files/04-plan.md
            - artifacts/flow-files/08-release.md
        write_file:
          path:
            - artifacts/flow-files/09-final-report.md
`)
	run, err := rt.StartFlow(context.Background(), FlowStartRequest{
		FlowPath:     flowPath,
		EntrypointID: "ad_hoc",
		Input: map[string]string{
			"request": "Build a file-backed Flow proof with skip-step reads, skill instructions, HITL, and persisted artifacts.",
		},
	})
	if err != nil {
		t.Fatalf("StartFlow() error = %v", err)
	}
	run = approveAndDrainLiveFlow(t, rt, run, 6*time.Minute)
	if run.Status != "completed" {
		t.Fatalf("file artifact flow status=%q current=%q, want completed", run.Status, run.CurrentStepID)
	}

	stepIDs := []string{"write_brief", "write_research", "write_constraints", "synthesize_plan", "approve_plan", "implementation_slice", "risk_review", "test_plan", "release_notes", "final_report"}
	for _, stepID := range stepIDs {
		if got := run.Steps[stepID].Status; got != "completed" {
			t.Fatalf("step %s status=%q, want completed", stepID, got)
		}
	}

	expectedFiles := map[string][]string{
		"artifacts/flow-files/01-brief.md":          {"BRIEF-MARKER-101"},
		"artifacts/flow-files/02-research.md":       {"RESEARCH-MARKER-202"},
		"artifacts/flow-files/03-constraints.md":    {"CONSTRAINT-MARKER-303"},
		"artifacts/flow-files/04-plan.md":           {"PLAN-MARKER-404", "BRIEF-MARKER-101", "CONSTRAINT-MARKER-303"},
		"artifacts/flow-files/05-implementation.md": {"IMPL-MARKER-505", "PLAN-MARKER-404"},
		"artifacts/flow-files/06-risk.md":           {"RISK-MARKER-606", "RESEARCH-MARKER-202", "IMPL-MARKER-505"},
		"artifacts/flow-files/07-test-plan.md":      {"TEST-MARKER-707", "BRIEF-MARKER-101", "RISK-MARKER-606"},
		"artifacts/flow-files/08-release.md":        {"RELEASE-MARKER-808", "CONSTRAINT-MARKER-303", "TEST-MARKER-707"},
		"artifacts/flow-files/09-final-report.md":   {"FINAL-MARKER-909", "BRIEF-MARKER-101", "PLAN-MARKER-404", "RELEASE-MARKER-808"},
	}
	for rel, markers := range expectedFiles {
		assertWorkspaceFileContains(t, rt, rel, markers...)
	}
	assertArtifactReferencesKnownFiles(t, rt, expectedFiles)
	assertInitialStepsDoNotClaimReads(t, rt, "artifacts/flow-files/04-plan.md")
	assertPlanToolContractTable(t, rt, "artifacts/flow-files/04-plan.md")

	toolCounts := map[string]int{}
	agentsSeen := map[string]bool{}
	for _, stepID := range []string{"write_brief", "write_research", "write_constraints", "synthesize_plan", "implementation_slice", "risk_review", "test_plan", "release_notes", "final_report"} {
		agentRunID := run.Steps[stepID].AgentRunID
		if strings.TrimSpace(agentRunID) == "" {
			t.Fatalf("step %s missing agent_run_id", stepID)
		}
		agentRun, err := rt.RunStore().Load(agentRunID)
		if err != nil {
			t.Fatalf("load agent run for step %s: %v", stepID, err)
		}
		agentsSeen[agentRun.AgentID] = true
		if !containsString(agentRun.ModelPolicy.Skills, "flow-file-artifact") {
			t.Fatalf("step %s run %s did not activate flow-file-artifact skill: %+v", stepID, agentRunID, agentRun.ModelPolicy.Skills)
		}
		if len(agentRun.ToolCalls) == 0 {
			t.Fatalf("step %s run %s recorded no tool calls", stepID, agentRunID)
		}
		assertPersistedToolCalls(t, rt, agentRunID, len(agentRun.ToolCalls))
		for _, call := range agentRun.ToolCalls {
			if call.Error != "" {
				t.Fatalf("step %s tool %s failed: %s input=%+v", stepID, call.Name, call.Error, call.Input)
			}
			toolCounts[call.Name]++
		}
	}
	for _, agentID := range []string{"flow-writer", "flow-research", "flow-architect", "flow-reviewer"} {
		if !agentsSeen[agentID] {
			t.Fatalf("agent %s was not used; seen=%v", agentID, agentsSeen)
		}
	}
	for _, toolName := range []string{"write_file", "read_file", "search_file", "list_dir"} {
		if toolCounts[toolName] == 0 {
			t.Fatalf("tool %s was not used; counts=%v", toolName, toolCounts)
		}
	}
	if toolCounts["write_file"] < len(expectedFiles) {
		t.Fatalf("write_file count=%d, want at least %d; counts=%v", toolCounts["write_file"], len(expectedFiles), toolCounts)
	}
	contracts := map[string]flowStepToolContract{
		"write_brief": {
			Names:      []string{"write_file"},
			WritePaths: []string{"artifacts/flow-files/01-brief.md"},
		},
		"write_research": {
			Names:      []string{"write_file"},
			WritePaths: []string{"artifacts/flow-files/02-research.md"},
		},
		"write_constraints": {
			Names:      []string{"write_file"},
			WritePaths: []string{"artifacts/flow-files/03-constraints.md"},
		},
		"synthesize_plan": {
			Names:      []string{"read_file", "read_file", "write_file"},
			ReadPaths:  []string{"artifacts/flow-files/01-brief.md", "artifacts/flow-files/03-constraints.md"},
			WritePaths: []string{"artifacts/flow-files/04-plan.md"},
		},
		"implementation_slice": {
			Names:      []string{"read_file", "write_file"},
			ReadPaths:  []string{"artifacts/flow-files/04-plan.md"},
			WritePaths: []string{"artifacts/flow-files/05-implementation.md"},
		},
		"risk_review": {
			Names:      []string{"read_file", "read_file", "write_file"},
			ReadPaths:  []string{"artifacts/flow-files/02-research.md", "artifacts/flow-files/05-implementation.md"},
			WritePaths: []string{"artifacts/flow-files/06-risk.md"},
		},
		"test_plan": {
			Names:         []string{"search_file", "read_file", "write_file"},
			ReadPaths:     []string{"artifacts/flow-files/06-risk.md"},
			SearchRoots:   []string{"artifacts/flow-files"},
			SearchQueries: []string{"BRIEF-MARKER-101"},
			WritePaths:    []string{"artifacts/flow-files/07-test-plan.md"},
		},
		"release_notes": {
			Names:      []string{"read_file", "read_file", "write_file"},
			ReadPaths:  []string{"artifacts/flow-files/03-constraints.md", "artifacts/flow-files/07-test-plan.md"},
			WritePaths: []string{"artifacts/flow-files/08-release.md"},
		},
		"final_report": {
			Names:      []string{"list_dir", "read_file", "read_file", "read_file", "write_file"},
			ListPaths:  []string{"artifacts/flow-files"},
			ReadPaths:  []string{"artifacts/flow-files/01-brief.md", "artifacts/flow-files/04-plan.md", "artifacts/flow-files/08-release.md"},
			WritePaths: []string{"artifacts/flow-files/09-final-report.md"},
		},
	}
	for stepID, contract := range contracts {
		assertStepToolContract(t, rt, run, stepID, contract)
	}
}

func newLiveDeepSeekHITLService(t *testing.T, allowWrite bool) *Service {
	t.Helper()
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek HITL smoke tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek HITL smoke tests")
	}
	model := strings.TrimSpace(os.Getenv("XIRA_DEEPSEEK_MODEL"))
	if model == "" {
		model = deepseek.ModelPro
	}
	if !deepseek.SupportedModel(model) {
		t.Fatalf("unsupported live DeepSeek model %q", model)
	}
	artifactRoot := strings.TrimSpace(os.Getenv("XIRA_LIVE_ARTIFACT_ROOT"))
	workspace := t.TempDir()
	runRoot := filepath.Join(t.TempDir(), "runs")
	stateRoot := filepath.Join(t.TempDir(), "state")
	if artifactRoot != "" {
		testRoot := liveDeepSeekTestRoot(t, artifactRoot)
		workspace = filepath.Join(testRoot, "workspace")
		runRoot = filepath.Join(testRoot, "runs")
		stateRoot = filepath.Join(testRoot, "state")
		t.Logf("preserving live HITL artifacts under %s", testRoot)
	}
	tools := ""
	if allowWrite {
		tools = `
tools:
  - write_file`
	}
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Live HITL smoke assistant.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
delegation:
  enabled: true
  allow:
    - research-assistant
  max_depth: 1
  max_parallel: 1
  max_outstanding: 4`+tools+`
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Follow the user's requested runtime tool call exactly for HITL smoke tests.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), `# Soul

Direct.
`)
	writeFile(t, filepath.Join(workspace, "agents", "research-assistant", "PROFILE.md"), `---
id: research-assistant
name: Research Assistant
version: 0.1.1
description: Live HITL smoke child assistant.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

For live delegation HITL smoke tests, call human.request exactly once when the parent task asks for child approval.
`)
	writeFile(t, filepath.Join(workspace, "agents", "research-assistant", "SOUL.md"), `# Soul

Careful.
`)
	return newTestService(t, Config{
		WorkspaceRoot: workspace,
		RunRoot:       runRoot,
		StateDir:      stateRoot,
		DeepSeekClient: deepseek.New(
			deepseek.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
		),
	})
}

func newLiveDeepSeekLongFlowService(t *testing.T) *Service {
	t.Helper()
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek long flow smoke tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek long flow smoke tests")
	}
	model := strings.TrimSpace(os.Getenv("XIRA_DEEPSEEK_MODEL"))
	if model == "" {
		model = deepseek.ModelPro
	}
	if !deepseek.SupportedModel(model) {
		t.Fatalf("unsupported live DeepSeek model %q", model)
	}
	artifactRoot := strings.TrimSpace(os.Getenv("XIRA_LIVE_ARTIFACT_ROOT"))
	workspace := t.TempDir()
	runRoot := filepath.Join(t.TempDir(), "runs")
	stateRoot := filepath.Join(t.TempDir(), "state")
	if artifactRoot != "" {
		testRoot := liveDeepSeekTestRoot(t, artifactRoot)
		workspace = filepath.Join(testRoot, "workspace")
		runRoot = filepath.Join(testRoot, "runs")
		stateRoot = filepath.Join(testRoot, "state")
		t.Logf("preserving live long flow artifacts under %s", testRoot)
	}
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Default assistant required by runtime bootstrap.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Default bootstrap agent. The long Flow live test does not route work here.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), `# Soul

Direct.
`)
	for _, agent := range []struct {
		id   string
		name string
		role string
	}{
		{id: "flow-intake", name: "Flow Intake", role: "You condense user requests into crisp execution briefs."},
		{id: "flow-research", name: "Flow Research", role: "You provide concise context, constraints, and final synthesis."},
		{id: "flow-architect", name: "Flow Architect", role: "You draft concise architecture and implementation plans."},
		{id: "flow-reviewer", name: "Flow Reviewer", role: "You identify risks, mitigations, and practical tests."},
	} {
		writeFile(t, filepath.Join(workspace, "agents", agent.id, "PROFILE.md"), `---
id: `+agent.id+`
name: `+agent.name+`
version: 0.1.1
description: Live long flow `+agent.name+`.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

`+agent.role+`
Follow the flow step instructions exactly. Keep outputs short and deterministic.
`)
		writeFile(t, filepath.Join(workspace, "agents", agent.id, "SOUL.md"), `# Soul

Direct and concise.
`)
	}
	return newTestService(t, Config{
		WorkspaceRoot: workspace,
		RunRoot:       runRoot,
		StateDir:      stateRoot,
		DeepSeekClient: deepseek.New(
			deepseek.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
		),
	})
}

func newLiveDeepSeekLongFlowToolService(t *testing.T) *Service {
	t.Helper()
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek long flow tool tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek long flow tool tests")
	}
	model := strings.TrimSpace(os.Getenv("XIRA_DEEPSEEK_MODEL"))
	if model == "" {
		model = deepseek.ModelPro
	}
	if !deepseek.SupportedModel(model) {
		t.Fatalf("unsupported live DeepSeek model %q", model)
	}
	artifactRoot := strings.TrimSpace(os.Getenv("XIRA_LIVE_ARTIFACT_ROOT"))
	workspace := t.TempDir()
	runRoot := filepath.Join(t.TempDir(), "runs")
	stateRoot := filepath.Join(t.TempDir(), "state")
	if artifactRoot != "" {
		testRoot := liveDeepSeekTestRoot(t, artifactRoot)
		workspace = filepath.Join(testRoot, "workspace")
		runRoot = filepath.Join(testRoot, "runs")
		stateRoot = filepath.Join(testRoot, "state")
		t.Logf("preserving live long flow tool artifacts under %s", testRoot)
	}
	writeFile(t, filepath.Join(workspace, "case", "request.md"), `# Request

REQ-MARKER-741
Build a 10-step Flow live test that proves agent tools are used, not only model text.
`)
	writeFile(t, filepath.Join(workspace, "case", "constraints.md"), `# Constraints

BUDGET-MARKER-284
Keep responses short. Preserve every replay artifact. The Flow must stop at approve_plan for human approval.
`)
	writeFile(t, filepath.Join(workspace, "case", "architecture.md"), `# Architecture

ARCH-MARKER-519
Use four agents: intake, research, architect, reviewer. Require read_file, list_dir, and search_file evidence.
`)
	writeFile(t, filepath.Join(workspace, "case", "risk.md"), `# Risk

RISK-MARKER-903
The model can hallucinate completed checkpoints unless tool_calls.jsonl and step outputs are inspected.
`)
	writeFile(t, filepath.Join(workspace, "case", "implementation.md"), `# Implementation

IMPL-MARKER-337
Assert each non-human Flow step records at least one tool call and that persisted JSONL records match the run.
`)
	writeFile(t, filepath.Join(workspace, "case", "test-plan.md"), `# Test Plan

TEST-MARKER-626
Replay the preserved state, human request, run YAML files, events, LLM calls, usage, and tool_calls JSONL.
`)
	writeFile(t, filepath.Join(workspace, "case", "release.md"), `# Release

REL-MARKER-812
Document the difference between Flow/HITL orchestration smoke and real tool-using Flow validation.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Default assistant required by runtime bootstrap.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Default bootstrap agent. The long Flow tool live test does not route work here.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), `# Soul

Direct.
`)
	for _, agent := range []struct {
		id         string
		name       string
		role       string
		tools      []string
		obligation string
	}{
		{
			id:         "flow-intake",
			name:       "Flow Intake",
			role:       "You turn source material into compact execution and release language.",
			tools:      []string{"read_file", "list_dir"},
			obligation: "When a step names a workspace path, call the requested tool before answering and cite the marker found in the tool output.",
		},
		{
			id:         "flow-research",
			name:       "Flow Research",
			role:       "You inspect workspace evidence and produce concise context or synthesis.",
			tools:      []string{"read_file", "search_file", "list_dir"},
			obligation: "Use search_file or list_dir exactly when the step asks for it. Do not invent files, markers, or checkpoints.",
		},
		{
			id:         "flow-architect",
			name:       "Flow Architect",
			role:       "You derive architecture and implementation slices from workspace files.",
			tools:      []string{"read_file", "list_dir"},
			obligation: "Read the requested source file before planning. Keep the answer tied to the marker in that file.",
		},
		{
			id:         "flow-reviewer",
			name:       "Flow Reviewer",
			role:       "You verify risks and test obligations from workspace evidence.",
			tools:      []string{"read_file", "search_file"},
			obligation: "Search for the requested marker before reviewing. Report only what the tool result supports.",
		},
	} {
		writeFile(t, filepath.Join(workspace, "agents", agent.id, "PROFILE.md"), `---
id: `+agent.id+`
name: `+agent.name+`
version: 0.1.1
description: Live long flow tool `+agent.name+`.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
tools:
`+yamlStringList(agent.tools, "  ")+`verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

`+agent.role+`

`+agent.obligation+`
Follow the Flow step instructions exactly. Keep outputs short and deterministic.
`)
		writeFile(t, filepath.Join(workspace, "agents", agent.id, "SOUL.md"), `# Soul

Ground every claim in the requested tool output.
`)
	}
	return newTestService(t, Config{
		WorkspaceRoot: workspace,
		RunRoot:       runRoot,
		StateDir:      stateRoot,
		DeepSeekClient: deepseek.New(
			deepseek.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
		),
	})
}

func newLiveDeepSeekFlowFileSkillService(t *testing.T) *Service {
	t.Helper()
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek file artifact Flow tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek file artifact Flow tests")
	}
	model := strings.TrimSpace(os.Getenv("XIRA_DEEPSEEK_MODEL"))
	if model == "" {
		model = deepseek.ModelPro
	}
	if !deepseek.SupportedModel(model) {
		t.Fatalf("unsupported live DeepSeek model %q", model)
	}
	artifactRoot := strings.TrimSpace(os.Getenv("XIRA_LIVE_ARTIFACT_ROOT"))
	workspace := t.TempDir()
	runRoot := filepath.Join(t.TempDir(), "runs")
	stateRoot := filepath.Join(t.TempDir(), "state")
	if artifactRoot != "" {
		testRoot := liveDeepSeekTestRoot(t, artifactRoot)
		workspace = filepath.Join(testRoot, "workspace")
		runRoot = filepath.Join(testRoot, "runs")
		stateRoot = filepath.Join(testRoot, "state")
		t.Logf("preserving live Flow file artifact artifacts under %s", testRoot)
	}
	writeFlowFileArtifactSkill(t, workspace)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Default assistant required by runtime bootstrap.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Default bootstrap agent. The file artifact Flow live test does not route work here.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), `# Soul

Direct.
`)
	for _, agent := range []struct {
		id   string
		name string
		role string
	}{
		{id: "flow-writer", name: "Flow Writer", role: "You create concise markdown artifacts exactly at the requested workspace paths."},
		{id: "flow-research", name: "Flow Research", role: "You inspect existing artifacts before writing synthesis files."},
		{id: "flow-architect", name: "Flow Architect", role: "You read prior decisions and write plan or implementation artifacts."},
		{id: "flow-reviewer", name: "Flow Reviewer", role: "You read non-adjacent evidence and write risk or test artifacts."},
	} {
		writeFile(t, filepath.Join(workspace, "agents", agent.id, "PROFILE.md"), `---
id: `+agent.id+`
name: `+agent.name+`
version: 0.1.1
description: Live file artifact Flow `+agent.name+`.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
tools:
  - read_file
  - search_file
  - write_file
  - list_dir
skills:
  - flow-file-artifact
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

`+agent.role+`

Use the loaded skill. Follow the Flow step instructions exactly. When asked to write a file, call `+"`write_file`"+` instead of only describing the file. When asked to read prior artifacts, call the requested read/search/list tools before writing.
The loaded skill text is already in this prompt. Do not inspect skill source directories during the Flow unless a step explicitly asks for it.
Use only artifact filenames explicitly named by the Flow definition. Do not invent shorthand or alternate filenames.
The only artifact filenames allowed in this live Flow are `+"`01-brief.md`"+`, `+"`02-research.md`"+`, `+"`03-constraints.md`"+`, `+"`04-plan.md`"+`, `+"`05-implementation.md`"+`, `+"`06-risk.md`"+`, `+"`07-test-plan.md`"+`, `+"`08-release.md`"+`, and `+"`09-final-report.md`"+`.
Business artifacts must not mention any filename outside that allowlist.
`)
		writeFile(t, filepath.Join(workspace, "agents", agent.id, "SOUL.md"), `# Soul

Ground every artifact in workspace files and keep confirmations short.
`)
	}
	return newTestService(t, Config{
		WorkspaceRoot: workspace,
		RunRoot:       runRoot,
		StateDir:      stateRoot,
		DeepSeekClient: deepseek.New(
			deepseek.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
		),
	})
}

func liveDeepSeekTestRoot(t *testing.T, artifactRoot string) string {
	t.Helper()
	name := strings.ToLower(t.Name())
	name = regexp.MustCompile(`[^a-z0-9_.-]+`).ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "test"
	}
	return filepath.Join(artifactRoot, name)
}

func writeFlowFileArtifactSkill(t *testing.T, workspace string) {
	t.Helper()
	skillDir := filepath.Join(workspace, "skills", "flow-file-artifact")
	writeFile(t, filepath.Join(skillDir, "SKILL.md"), `---
schema_version: xira.skill.v0
id: flow-file-artifact
name: Flow File Artifact
version: 0.1.0
description: Produce replayable Flow artifacts by reading and writing workspace files.
activation:
  mode: explicit
requires:
  tools:
    - read_file
    - search_file
    - write_file
    - list_dir
context:
  includes:
    - references/
verification:
  default_checks:
    - final_response_non_empty
artifacts:
  output_dir: artifacts/flow-files
  retention: local
---
# Instructions

For Flow file artifact steps:

- Use the exact workspace paths requested by the Flow step.
- Use `+"`write_file`"+` for deliverables; do not merely describe what would be written.
- Before writing a derived artifact, read or search every prior artifact named by the step.
- Do not inspect the skill directory at runtime; these instructions are already loaded into the agent prompt.
- Do not call extra tools for curiosity. Use only the tools named by the current Flow step.
- Use only the artifact filenames explicitly named in the Flow. Never invent, abbreviate, or rename artifact files.
- The complete allowed artifact filename set is: `+"`01-brief.md`"+`, `+"`02-research.md`"+`, `+"`03-constraints.md`"+`, `+"`04-plan.md`"+`, `+"`05-implementation.md`"+`, `+"`06-risk.md`"+`, `+"`07-test-plan.md`"+`, `+"`08-release.md`"+`, `+"`09-final-report.md`"+`.
- Produced business artifacts must not mention any filename outside that allowlist.
- Preserve marker strings verbatim so replay tests can verify cross-step provenance.
- Keep the final chat response short after a write is approved.
`)
	writeFile(t, filepath.Join(skillDir, "references", "artifact-contract.md"), `# Artifact Contract

Every produced file must be replay-friendly: include the step marker, upstream markers requested by the Flow step, and enough short context to prove the file was derived from the requested prior artifacts.
`)
}

func assertPersistedToolCalls(t *testing.T, rt *Service, runID string, want int) {
	t.Helper()
	path := filepath.Join(rt.RunStore().root, runID, "tool_calls.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted tool calls for run %s: %v", runID, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		t.Fatalf("persisted tool calls for run %s are empty at %s", runID, path)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != want {
		t.Fatalf("persisted tool call lines for run %s = %d, want %d", runID, len(lines), want)
	}
	for i, line := range lines {
		var rec ToolCallRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode persisted tool call %d for run %s: %v line=%q", i+1, runID, err, line)
		}
		if strings.TrimSpace(rec.Name) == "" {
			t.Fatalf("persisted tool call %d for run %s missing name: %+v", i+1, runID, rec)
		}
	}
}

func approveAndDrainLiveFlow(t *testing.T, rt *Service, run *FlowRun, timeout time.Duration) *FlowRun {
	t.Helper()
	var err error
	deadline := time.Now().Add(timeout)
	for turn := 1; time.Now().Before(deadline) && run.Status != "completed"; turn++ {
		if run.Status == StatusWaitingHuman {
			ids := pendingFlowHumanRequestIDs(run)
			if len(ids) == 0 {
				t.Fatalf("flow %s waiting_human at %q with no pending human request ids", run.ID, run.CurrentStepID)
			}
			req, err := rt.GetHumanRequest(context.Background(), ids[0])
			if err != nil {
				t.Fatalf("GetHumanRequest(%s) error = %v", ids[0], err)
			}
			if req.Status == humanrequest.StatusPending {
				if _, err := rt.ResolveHumanRequest(context.Background(), req.ID, humanrequestApprove("live-file-flow")); err != nil {
					t.Fatalf("ResolveHumanRequest(%s) error = %v", req.ID, err)
				}
			}
			run, err = rt.ResumeFlow(context.Background(), run.ID, req.ID)
			if err != nil {
				t.Fatalf("ResumeFlow(%s, %s) error = %v", run.ID, req.ID, err)
			}
			if run.Status == StatusWaitingHuman {
				time.Sleep(500 * time.Millisecond)
				reloaded, err := rt.GetFlowRun(context.Background(), run.ID)
				if err != nil {
					t.Fatalf("GetFlowRun(%s) after resume error = %v", run.ID, err)
				}
				run = reloaded
			}
			continue
		}
		run, err = rt.AdvanceFlow(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("AdvanceFlow turn %d error = %v", turn, err)
		}
	}
	return run
}

func pendingFlowHumanRequestIDs(run *FlowRun) []string {
	if run == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var ids []string
	add := func(values []string) {
		for _, id := range values {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	add(run.PendingHumanRequests)
	if step, ok := run.Steps[run.CurrentStepID]; ok {
		add(step.HumanRequestIDs)
	}
	return ids
}

func assertWorkspaceFileContains(t *testing.T, rt *Service, rel string, markers ...string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rt.workspace, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read workspace file %s: %v", rel, err)
	}
	content := string(data)
	for _, marker := range markers {
		if !strings.Contains(content, marker) {
			t.Fatalf("workspace file %s missing marker %q:\n%s", rel, marker, content)
		}
	}
}

type flowStepToolContract struct {
	Names         []string
	ReadPaths     []string
	WritePaths    []string
	SearchRoots   []string
	SearchQueries []string
	ListPaths     []string
}

func assertStepToolContract(t *testing.T, rt *Service, flowRun *FlowRun, stepID string, contract flowStepToolContract) {
	t.Helper()
	step, ok := flowRun.Steps[stepID]
	if !ok {
		t.Fatalf("step %s missing in flow run", stepID)
	}
	run, err := rt.RunStore().Load(step.AgentRunID)
	if err != nil {
		t.Fatalf("load agent run for step %s: %v", stepID, err)
	}
	var names []string
	for _, call := range run.ToolCalls {
		names = append(names, call.Name)
	}
	allowedNames := stringSet(contract.Names)
	for _, name := range names {
		if _, ok := allowedNames[name]; !ok {
			t.Fatalf("step %s tool %q is not allowed; names=%v allowed=%v tool_calls=%+v", stepID, name, names, contract.Names, run.ToolCalls)
		}
	}
	if len(contract.WritePaths) > 0 {
		last := run.ToolCalls[len(run.ToolCalls)-1]
		if last.Name != "write_file" {
			t.Fatalf("step %s last tool=%q, want write_file after prerequisite tools; tool_calls=%+v", stepID, last.Name, run.ToolCalls)
		}
	}
	assertToolInputValuesAtLeastAllowed(t, stepID, run.ToolCalls, "read_file", "path", contract.ReadPaths)
	assertToolInputValuesExact(t, stepID, run.ToolCalls, "write_file", "path", contract.WritePaths)
	assertToolInputValuesAtLeastAllowed(t, stepID, run.ToolCalls, "search_file", "root", contract.SearchRoots)
	assertToolInputValuesAtLeastAllowed(t, stepID, run.ToolCalls, "search_file", "query", contract.SearchQueries)
	assertToolInputValuesAtLeastAllowed(t, stepID, run.ToolCalls, "list_dir", "path", contract.ListPaths)
}

func assertToolInputValuesExact(t *testing.T, stepID string, calls []ToolCallRecord, toolName, inputKey string, want []string) {
	t.Helper()
	var got []string
	for _, call := range calls {
		if call.Name != toolName {
			continue
		}
		got = append(got, strings.TrimSpace(fmt.Sprint(call.Input[inputKey])))
	}
	if !stringSlicesEqual(got, want) {
		if !stringSlicesEqualUnordered(got, want) {
			t.Fatalf("step %s %s.%s=%v, want exact set %v; tool_calls=%+v", stepID, toolName, inputKey, got, want, calls)
		}
	}
}

func assertToolInputValuesAtLeastAllowed(t *testing.T, stepID string, calls []ToolCallRecord, toolName, inputKey string, want []string) {
	t.Helper()
	allowed := stringSet(want)
	seen := map[string]struct{}{}
	for _, call := range calls {
		if call.Name != toolName {
			continue
		}
		got := strings.TrimSpace(fmt.Sprint(call.Input[inputKey]))
		if _, ok := allowed[got]; !ok {
			t.Fatalf("step %s %s.%s=%q is not allowed; want values %v; tool_calls=%+v", stepID, toolName, inputKey, got, want, calls)
		}
		seen[got] = struct{}{}
	}
	for _, value := range want {
		if _, ok := seen[value]; !ok {
			t.Fatalf("step %s missing %s.%s=%q; seen=%v tool_calls=%+v", stepID, toolName, inputKey, value, sortedStringSetKeys(seen), calls)
		}
	}
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSlicesEqualUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]string(nil), a...)
	right := append([]string(nil), b...)
	sort.Strings(left)
	sort.Strings(right)
	return stringSlicesEqual(left, right)
}

func assertArtifactReferencesKnownFiles(t *testing.T, rt *Service, expected map[string][]string) {
	t.Helper()
	known := map[string]struct{}{}
	for rel := range expected {
		known[filepath.Base(filepath.FromSlash(rel))] = struct{}{}
	}
	refPattern := regexp.MustCompile(`\b(?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.md\b`)
	for rel := range expected {
		data, err := os.ReadFile(filepath.Join(rt.workspace, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read workspace file %s: %v", rel, err)
		}
		for _, ref := range refPattern.FindAllString(string(data), -1) {
			base := filepath.Base(filepath.FromSlash(ref))
			if _, ok := known[base]; !ok {
				t.Fatalf("workspace file %s references unknown markdown artifact %q; known=%v\n%s", rel, ref, sortedStringSetKeys(known), string(data))
			}
		}
	}
}

func assertInitialStepsDoNotClaimReads(t *testing.T, rt *Service, rel string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rt.workspace, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read workspace file %s: %v", rel, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		normalized := strings.ToLower(line)
		for _, stepID := range []string{"write_brief", "write_research", "write_constraints"} {
			if strings.Contains(normalized, stepID) && claimsPositiveReadSearchOrList(normalized) {
				t.Fatalf("workspace file %s incorrectly claims initial step %s used read/search/list:\n%s", rel, stepID, string(data))
			}
		}
	}
}

func claimsPositiveReadSearchOrList(line string) bool {
	line = strings.NewReplacer("*", " ", "_", " ", "`", " ").Replace(line)
	line = strings.Join(strings.Fields(line), " ")
	for _, negation := range []string{"does not", "doesn't", "do not", "don't", "did not", "didn't", "no read/search/list", "no read", "no dependency", "no dependencies", "none", "without"} {
		if strings.Contains(line, negation) {
			return false
		}
	}
	nearbyNegationPatterns := []*regexp.Regexp{
		regexp.MustCompile(`\b(no|not|without)\b.{0,48}\b(read|reads|read_file|search|searches|search_file|list|lists|list_dir)\b`),
		regexp.MustCompile(`\b(read|reads|read_file|search|searches|search_file|list|lists|list_dir)\b.{0,48}\b(no|not|without)\b`),
	}
	for _, pattern := range nearbyNegationPatterns {
		if pattern.MatchString(line) {
			return false
		}
	}
	positivePatterns := []*regexp.Regexp{
		regexp.MustCompile(`\b(reads?|searches?|lists?)\b`),
		regexp.MustCompile(`\bcalls?\s+(read|search|list)`),
		regexp.MustCompile(`\buses?\s+(read|search|list)`),
	}
	for _, pattern := range positivePatterns {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
}

func TestClaimsPositiveReadSearchOrListAllowsNegatedDeepSeekPlanWording(t *testing.T) {
	lines := []string{
		"write_brief, write_research, and write_constraints do not call read_file, search_file, or list_dir.",
		"1. **write_brief** - Produces `01-brief.md`. Does not call `read_file`, `search_file`, or `list_dir`. No upstream dependencies.",
		"- Steps `write_brief`, `write_research`, and `write_constraints` are independent and require no prior reads.",
		"| write_brief | no read/search/list | 01-brief.md |",
		"- **write_constraints**: no dependency on 01-brief.md or 02-research.md (skip-step reads).",
	}
	for _, line := range lines {
		if claimsPositiveReadSearchOrList(strings.ToLower(line)) {
			t.Fatalf("negated line was classified as positive read/search/list claim: %q", line)
		}
	}
	positiveLines := []string{
		"write_research reads 01-brief.md before writing.",
		"write_constraints calls read_file for 02-research.md.",
	}
	for _, line := range positiveLines {
		if !claimsPositiveReadSearchOrList(strings.ToLower(line)) {
			t.Fatalf("positive line was not classified as read/search/list claim: %q", line)
		}
	}
}

func assertPlanToolContractTable(t *testing.T, rt *Service, rel string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rt.workspace, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read workspace file %s: %v", rel, err)
	}
	content := strings.ToLower(strings.Join(strings.Fields(string(data)), " "))
	expectedRows := []string{
		"| write_brief | no read/search/list | 01-brief.md |",
		"| write_research | no read/search/list | 02-research.md |",
		"| write_constraints | no read/search/list | 03-constraints.md |",
		"| synthesize_plan | read 01-brief.md and 03-constraints.md | 04-plan.md |",
		"| implementation_slice | read 04-plan.md | 05-implementation.md |",
		"| risk_review | read 02-research.md and 05-implementation.md | 06-risk.md |",
		"| test_plan | search brief-marker-101 under artifacts/flow-files; read 06-risk.md | 07-test-plan.md |",
		"| release_notes | read 03-constraints.md and 07-test-plan.md | 08-release.md |",
		"| final_report | list artifacts/flow-files; read 01-brief.md, 04-plan.md, and 08-release.md | 09-final-report.md |",
	}
	for _, row := range expectedRows {
		normalizedRow := strings.ToLower(strings.Join(strings.Fields(row), " "))
		if !strings.Contains(content, normalizedRow) {
			t.Fatalf("workspace file %s missing expected tool contract row %q:\n%s", rel, row, string(data))
		}
	}
}

func sortedStringSetKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func assertRunReadToolPath(t *testing.T, rt *Service, runID, rel string) {
	t.Helper()
	run, err := rt.RunStore().Load(runID)
	if err != nil {
		t.Fatalf("load run %s: %v", runID, err)
	}
	for _, call := range run.ToolCalls {
		if call.Name != "read_file" {
			continue
		}
		if got, _ := call.Input["path"].(string); got == rel {
			return
		}
	}
	t.Fatalf("run %s did not read %s; tool_calls=%+v", runID, rel, run.ToolCalls)
}

func humanrequestApprove(actor string) humanrequest.ResolveRequest {
	return humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   actor,
		Message: "approved",
	}
}

// newLiveDeepSeekFlowRegistryService constructs a Service whose workspace
// contains flows/hello/flow.yaml (the registry-discovered layout) and a single
// xira-assistant agent. It is intentionally separate from
// newLiveDeepSeekFlowFileSkillService: that helper writes a flow to an ad-hoc
// path and materializes flow-* agents, whereas this one exercises the registry
// (flow started by id, not by path) with the bootstrap agent only.
func newLiveDeepSeekFlowRegistryService(t *testing.T) *Service {
	t.Helper()
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek flow registry tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek flow registry tests")
	}
	model := strings.TrimSpace(os.Getenv("XIRA_DEEPSEEK_MODEL"))
	if model == "" {
		model = deepseek.ModelPro
	}
	if !deepseek.SupportedModel(model) {
		t.Fatalf("unsupported live DeepSeek model %q", model)
	}

	artifactRoot := strings.TrimSpace(os.Getenv("XIRA_LIVE_ARTIFACT_ROOT"))
	workspace := t.TempDir()
	runRoot := filepath.Join(t.TempDir(), "runs")
	stateRoot := filepath.Join(t.TempDir(), "state")
	if artifactRoot != "" {
		testRoot := liveDeepSeekTestRoot(t, artifactRoot)
		workspace = filepath.Join(testRoot, "workspace")
		runRoot = filepath.Join(testRoot, "runs")
		stateRoot = filepath.Join(testRoot, "state")
		t.Logf("preserving live flow registry artifacts under %s", testRoot)
	}

	// Bootstrap agent required by the runtime and used by the registry flow.
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Live flow registry bootstrap assistant.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Answer the user's request directly and concisely.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), "# Soul\n\nDirect.\n")

	// Registry-discovered flow: workspace/flows/hello/flow.yaml, id: hello.
	writeFile(t, filepath.Join(workspace, "flows", "hello", "flow.yaml"), `schema_version: xira.flow.v0
id: hello
name: Hello
version: 0.1.0
description: Live flow registry smoke flow.
objective: Answer the request in one step.
entrypoints:
  - id: ad_hoc
    start_step: answer
    required_inputs:
      - request
steps:
  - id: answer
    objective: Answer ${input.request}. End the reply with the token REGISTRY-LIVE-OK.
    executor:
      agent: xira-assistant
`)

	return newTestService(t, Config{
		WorkspaceRoot: workspace,
		RunRoot:       runRoot,
		StateDir:      stateRoot,
		DeepSeekClient: deepseek.New(
			deepseek.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
		),
	})
}

// TestRealDeepSeekFlowRegistryStartsByID starts a flow by registered id (no
// FlowPath) against the live DeepSeek model and asserts:
//   - the registry resolved the id (run.FlowID == "hello");
//   - the step completes with a non-empty response;
//   - the agent session messages land in the real trigger channel (cli), NOT
//     under a fabricated "flow" channel — this is the core contract that Plan A
//     (PR #10) unified and that the registry must not regress.
func TestRealDeepSeekFlowRegistryStartsByID(t *testing.T) {
	rt := newLiveDeepSeekFlowRegistryService(t)

	// Confirm the registry actually discovered the flow before starting.
	if _, ok := rt.FlowRegistry().Find("hello"); !ok {
		t.Fatalf("registry did not discover flow \"hello\"; refs=%+v", rt.FlowRefs())
	}

	run, err := rt.StartFlow(context.Background(), FlowStartRequest{
		FlowID:       "hello", // registry resolution, no FlowPath
		EntrypointID: "ad_hoc",
		Input:        map[string]string{"request": "registry live smoke"},
	})
	if err != nil {
		t.Fatalf("StartFlow by id: %v", err)
	}
	if run.FlowID != "hello" {
		t.Fatalf("run.FlowID = %q, want hello", run.FlowID)
	}

	run, err = rt.AdvanceFlow(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("AdvanceFlow: %v", err)
	}
	if run.Status != "completed" {
		t.Fatalf("run status = %q, want completed; steps=%+v", run.Status, run.Steps)
	}

	// Find the agent run produced by the step.
	var agentRunID string
	for _, step := range run.Steps {
		if step.AgentRunID != "" {
			agentRunID = step.AgentRunID
			break
		}
	}
	if agentRunID == "" {
		t.Fatalf("no agent_run_id in flow steps: %+v", run.Steps)
	}
	agentRun, err := rt.RunStore().Load(agentRunID)
	if err != nil {
		t.Fatalf("load agent run %s: %v", agentRunID, err)
	}
	if strings.TrimSpace(agentRun.FinalResponse) == "" {
		t.Fatalf("agent run %s has empty final response", agentRunID)
	}

	// Assert the session messages.jsonl exists and is NOT under a "flow"
	// channel subtree. A CLI-triggered flow must land under sessions/cli/...
	// (the unified identity contract from PR #10).
	sessionRoot := rt.SessionManager().Root()
	var messagesPath string
	_ = filepath.Walk(sessionRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) == "messages.jsonl" {
			messagesPath = path
		}
		return nil
	})
	if messagesPath == "" {
		t.Fatalf("no messages.jsonl found under session root %s", sessionRoot)
	}
	// Normalize separators so the assertion is cross-platform.
	normalized := filepath.ToSlash(messagesPath)
	if strings.Contains(normalized, "/flow/") {
		t.Fatalf("session messages landed under fabricated \"flow\" channel: %s\nwant the real trigger channel (e.g. cli)", messagesPath)
	}
	t.Logf("registry flow session messages persisted at %s", messagesPath)
}
