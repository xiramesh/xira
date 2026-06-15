package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

func TestRealDeepSeekHITLHumanRequestTool(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Live HITL smoke: call human.request exactly once and ask `Approve shipping HITL v0 smoke test?`. Do not answer normally.",
		Channel: "test",
		UserID:  "live-user",
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
		Channel: "test",
		UserID:  "live-user",
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
		Channel: "test",
		UserID:  "live-user",
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
		Channel: "test",
		UserID:  "live-user",
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
		Channel: "test",
		UserID:  "live-user",
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || resp.Interrupt == nil || len(resp.Interrupt.DelegationJoinIDs) != 1 || len(resp.HumanRequests) != 1 {
		t.Fatalf("live delegate response status=%q joins=%+v human_requests=%d final=%q", resp.Status, resp.Interrupt, len(resp.HumanRequests), resp.FinalResponse)
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
	workspace := t.TempDir()
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
		RunRoot:       filepath.Join(t.TempDir(), "runs"),
		StateRoot:     filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(
			deepseek.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
		),
	})
}

func humanrequestApprove(actor string) humanrequest.ResolveRequest {
	return humanrequest.ResolveRequest{
		Kind:    humanrequest.ResponseApprove,
		Actor:   actor,
		Message: "approved",
	}
}
