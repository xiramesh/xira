package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/flow"
	"github.com/xiramesh/xira/internal/humanrequest"
	frt "github.com/xiramesh/xira/internal/runtime"
)

// writeAPIFlowFile writes a minimal two-step flow using the default agent.
func writeAPIFlowFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "flow.yaml")
	content := `schema_version: xira.flow.v0
id: api-test
name: API Test
version: 0.1.0
objective: exercise the flow API surface
entrypoints:
  - id: ad_hoc
    start_step: only
steps:
  - id: only
    objective: Produce a task spec.
    executor:
      agent: xira-assistant
    output_contract:
      required_slots:
        - id: task_spec
    transitions:
      on_success: done
  - id: done
    objective: Report.
    executor:
      agent: xira-assistant
    output_contract:
      required_slots:
        - id: final_report
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write flow file: %v", err)
	}
	return path
}

func writeAPIRequiredInputFlowFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "flow-required.yaml")
	content := `schema_version: xira.flow.v0
id: api-required-test
name: API Required Input Test
version: 0.1.0
objective: reject incomplete start input
entrypoints:
  - id: ad_hoc
    start_step: only
    required_inputs:
      - request
steps:
  - id: only
    objective: Produce a task spec.
    executor:
      agent: xira-assistant
    output_contract:
      required_slots:
        - id: task_spec
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write required flow file: %v", err)
	}
	return path
}

func newFlowAPIServer(t *testing.T) (*Server, *frt.Service, string) {
	t.Helper()
	cfg := frt.Config{
		StateDir: filepath.Join(t.TempDir(), "state"),
	}
	rt := newAPITestService(t, cfg)
	server := NewServer(rt, "127.0.0.1:0")
	flowPath := writeAPIFlowFile(t, t.TempDir())
	return server, rt, flowPath
}

// newFlowRegistryAPIServer constructs a server whose runtime workspace contains
// flows/<id>/flow.yaml so the registry discovers them. It writes a minimal
// xira-assistant PROFILE.md so NewService loads agents from that workspace.
func newFlowRegistryAPIServer(t *testing.T, ids ...string) (*Server, *frt.Service) {
	t.Helper()
	workspace := t.TempDir()
	writeAPIAgentProfile(t, filepath.Join(workspace, "agents", "xira-assistant"))
	for _, id := range ids {
		writeAPIRegisteredFlowFile(t, workspace, id)
	}
	cfg := frt.Config{
		WorkspaceRoot: workspace,
		StateDir:      filepath.Join(t.TempDir(), "state"),
	}
	rt := newAPITestService(t, cfg)
	return NewServer(rt, "127.0.0.1:0"), rt
}

func writeAPIAgentProfile(t *testing.T, dir string) {
	t.Helper()
	content := `---
id: xira-assistant
name: Xira Assistant
version: 0.1.0
description: Default assistant.
model_policy:
  provider: deepseek
  model: test-model
  stream: false
---
# Instructions
Answer.
`
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir agent dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PROFILE.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write agent profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("# Soul\n\nDirect.\n"), 0o644); err != nil {
		t.Fatalf("write agent soul: %v", err)
	}
}

func writeAPIRegisteredFlowFile(t *testing.T, workspace, id string) {
	t.Helper()
	path := filepath.Join(workspace, "flows", id, "flow.yaml")
	content := `schema_version: xira.flow.v0
id: ` + id + `
name: ` + id + `
version: 0.1.0
entrypoints:
  - id: ad_hoc
    start_step: answer
    required_inputs:
      - request
steps:
  - id: answer
    objective: Answer ${input.request}.
    executor:
      agent: xira-assistant
`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir flow dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write flow file: %v", err)
	}
}

func TestGetFlowsListsRegisteredFlows(t *testing.T) {
	server, _ := newFlowRegistryAPIServer(t, "hello", "world")

	resp := serveJSON(t, server, http.MethodGet, "/api/v1/flows", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var body struct {
		Flows []frt.FlowRef `json:"flows"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Body.String())
	}
	if len(body.Flows) != 2 {
		t.Fatalf("flows len = %d, want 2: %s", len(body.Flows), resp.Body.String())
	}
	if body.Flows[0].ID != "hello" || body.Flows[1].ID != "world" {
		t.Fatalf("flows order = %+v, want [hello world]", body.Flows)
	}
}

func TestGetFlowsEmptyWhenNoFlows(t *testing.T) {
	server, _ := newFlowRegistryAPIServer(t)

	resp := serveJSON(t, server, http.MethodGet, "/api/v1/flows", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var body struct {
		Flows []frt.FlowRef `json:"flows"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Flows) != 0 {
		t.Fatalf("flows len = %d, want 0", len(body.Flows))
	}
}

func TestPostFlowRunAcceptsFlowID(t *testing.T) {
	server, _ := newFlowRegistryAPIServer(t, "hello")

	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs", map[string]any{
		"flow_id":       "hello",
		"entrypoint_id": "ad_hoc",
		"input":         map[string]string{"request": "hi"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var run frt.FlowRunView
	if err := json.Unmarshal(resp.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Body.String())
	}
	if run.FlowID != "hello" {
		t.Fatalf("flow_id = %q, want hello", run.FlowID)
	}
}

func TestPostFlowRunStartsRun(t *testing.T) {
	server, rt, flowPath := newFlowAPIServer(t)
	_ = rt
	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs", map[string]any{
		"flow_path":     flowPath,
		"entrypoint_id": "ad_hoc",
		"input":         map[string]string{"request": "x"},
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var run frt.FlowRunView
	if err := json.Unmarshal(resp.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Body.String())
	}
	if run.FlowID != "api-test" {
		t.Errorf("flow_id = %q", run.FlowID)
	}
	if run.Status != "running" {
		t.Errorf("status = %q, want running", run.Status)
	}
	if run.CurrentStepID != "only" {
		t.Errorf("current_step_id = %q, want only", run.CurrentStepID)
	}
}

func TestPostFlowRunRejectsUnknownEntrypoint(t *testing.T) {
	server, _, flowPath := newFlowAPIServer(t)
	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs", map[string]any{
		"flow_path":     flowPath,
		"entrypoint_id": "does_not_exist",
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
	var errBody map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg, _ := errBody["error"].(string); msg == "" {
		t.Fatalf("expected error body for unknown entrypoint, got %s", resp.Body.String())
	}
	if ct := resp.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
}

func TestPostFlowRunRejectsMissingRequiredInput(t *testing.T) {
	server, _, _ := newFlowAPIServer(t)
	flowPath := writeAPIRequiredInputFlowFile(t, t.TempDir())
	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs", map[string]any{
		"flow_path":     flowPath,
		"entrypoint_id": "ad_hoc",
		"input":         map[string]string{},
	})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", resp.Code, resp.Body.String())
	}
	var errBody map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg, _ := errBody["error"].(string); !strings.Contains(msg, "missing required") || !strings.Contains(msg, "request") {
		t.Fatalf("error = %q, want missing required request input", msg)
	}
}

func TestGetFlowRun(t *testing.T) {
	server, rt, flowPath := newFlowAPIServer(t)
	run, err := rt.StartFlow(context.Background(), frt.FlowStartRequest{
		FlowPath: flowPath, EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/flows/runs/"+run.ID, nil)
	resp := httptest.NewRecorder()
	server.server.Handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d", resp.Code)
	}
	var got frt.FlowRunView
	if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != run.ID {
		t.Errorf("id = %q, want %q", got.ID, run.ID)
	}
}

func TestPostFlowRunAdvance(t *testing.T) {
	server, rt, flowPath := newFlowAPIServer(t)
	run, err := rt.StartFlow(context.Background(), frt.FlowStartRequest{
		FlowPath: flowPath, EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs/"+run.ID+"/advance", map[string]any{})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var advanced frt.FlowRunView
	if err := json.Unmarshal(resp.Body.Bytes(), &advanced); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Body.String())
	}
	if advanced.CurrentStepID != "done" {
		t.Errorf("current_step_id = %q, want done", advanced.CurrentStepID)
	}
	if advanced.Steps["only"].Status != "completed" {
		t.Errorf("only status = %q, want completed", advanced.Steps["only"].Status)
	}
}

func TestPostFlowRunResume(t *testing.T) {
	server, rt, flowPath := newFlowAPIServer(t)
	run, err := rt.StartFlow(context.Background(), frt.FlowStartRequest{
		FlowPath: flowPath, EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Resume with a fake human request id; expect an error response because
	// the run is not waiting_human, but the endpoint must accept the request
	// shape and return the unchanged run (idempotent resume).
	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs/"+run.ID+"/resume", map[string]any{
		"human_request_id": "hrq_none",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	var resumed frt.FlowRunView
	if err := json.Unmarshal(resp.Body.Bytes(), &resumed); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Body.String())
	}
	if resumed.ID != run.ID {
		t.Errorf("id = %q", resumed.ID)
	}
}

func TestPostFlowRunResumeRejectsMissingHumanRequestID(t *testing.T) {
	server, rt, flowPath := newFlowAPIServer(t)
	run, err := rt.StartFlow(context.Background(), frt.FlowStartRequest{
		FlowPath: flowPath, EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs/"+run.ID+"/resume", map[string]any{})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Code)
	}
}

func TestPostFlowRunAdvanceUnknownRunReturns404(t *testing.T) {
	server, _, _ := newFlowAPIServer(t)
	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs/fr_missing/advance", map[string]any{})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", resp.Code, resp.Body.String())
	}
}

func TestPostFlowRunResumeUnknownLinkedHumanRequestReturns404(t *testing.T) {
	server, rt, flowPath := newFlowAPIServer(t)
	run, err := rt.StartFlow(context.Background(), frt.FlowStartRequest{
		FlowPath: flowPath, EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.FlowKernel().Store.UpdateRun(context.Background(), run.ID, func(r *frt.FlowRun) error {
		r.Status = "waiting_human"
		r.CurrentStepID = "only"
		s := r.Steps["only"]
		s.Status = "waiting_human"
		s.HumanRequestIDs = []string{"hrq_pending"}
		r.Steps["only"] = s
		r.PendingHumanRequests = []string{"hrq_pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs/"+run.ID+"/resume", map[string]any{"human_request_id": "hrq_pending"})
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown linked request; body = %s", resp.Code, resp.Body.String())
	}
}

func TestPostFlowRunResumePendingHumanRequestReturns409(t *testing.T) {
	server, rt, flowPath := newFlowAPIServer(t)
	run, err := rt.StartFlow(context.Background(), frt.FlowStartRequest{
		FlowPath: flowPath, EntrypointID: "ad_hoc", Input: map[string]string{"request": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := rt.CreateHumanRequest(context.Background(), humanrequest.CreateRequest{
		RunID:      run.ID,
		AgentID:    "flow:api-test",
		SessionID:  "flow:" + run.ID,
		ToolCallID: "flow_human_approval:" + run.ID + ":only",
		Source:     flow.SourceFlowHumanApproval,
		Kind:       humanrequest.RequestApproval,
		Question:   "approve?",
		Options: []humanrequest.HumanOption{
			{ID: "approve", Label: "approve"},
			{ID: "reject", Label: "reject"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.FlowKernel().Store.UpdateRun(context.Background(), run.ID, func(r *frt.FlowRun) error {
		r.Status = "waiting_human"
		r.CurrentStepID = "only"
		s := r.Steps["only"]
		s.Status = "waiting_human"
		s.HumanRequestIDs = []string{req.ID}
		r.Steps["only"] = s
		r.PendingHumanRequests = []string{req.ID}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := serveJSON(t, server, http.MethodPost, "/api/v1/flows/runs/"+run.ID+"/resume", map[string]any{"human_request_id": req.ID})
	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for pending linked request; body = %s", resp.Code, resp.Body.String())
	}
}
