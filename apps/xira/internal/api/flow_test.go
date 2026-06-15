package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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

func newFlowAPIServer(t *testing.T) (*Server, *frt.Service, string) {
	t.Helper()
	cfg := frt.Config{
		RunRoot:   filepath.Join(t.TempDir(), "runs"),
		StateRoot: filepath.Join(t.TempDir(), "state"),
	}
	rt := newAPITestService(t, cfg)
	server := NewServer(rt, "127.0.0.1:0")
	flowPath := writeAPIFlowFile(t, t.TempDir())
	return server, rt, flowPath
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
	if resp.Code != http.StatusOK {
		// Endpoint returns 200 with an error body for start failures.
	}
	var errBody map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg, _ := errBody["error"].(string); msg == "" {
		t.Fatalf("expected error body for unknown entrypoint, got %s", resp.Body.String())
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
