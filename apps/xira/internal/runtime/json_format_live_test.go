package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

// TestRealDeepSeekJSONResponseFormat proves the full PROFILE.md -> runtime ->
// ADK -> DeepSeek -> validated public final chain against the real provider.
// The user message intentionally does not mention JSON: the runtime-owned
// response-format instruction must satisfy DeepSeek's JSON-mode prompt rule.
func TestRealDeepSeekJSONResponseFormat(t *testing.T) {
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek JSON format tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek JSON format tests")
	}
	model := strings.TrimSpace(os.Getenv("XIRA_DEEPSEEK_MODEL"))
	if model == "" {
		model = deepseek.ModelPro
	}
	if !deepseek.SupportedModel(model) {
		t.Fatalf("unsupported live DeepSeek model %q", model)
	}

	workspace := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "state")
	writeFile(t, filepath.Join(workspace, "agents", "structured-assistant", "PROFILE.md"), `---
id: structured-assistant
name: Structured Assistant
version: 0.1.0
description: Live structured-response smoke assistant.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
  format: json
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Return an object containing exactly two fields: status set to ok and count set to 2.
`)
	writeFile(t, filepath.Join(workspace, "agents", "structured-assistant", "SOUL.md"), "# Soul\n\nPrecise.\n")
	writeFile(t, filepath.Join(workspace, "xira.yaml"), `workspace: .
default_agent: structured-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(workspace, "entrypoints.yaml"), `entrypoints:
  - id: test-default
    channel: test
    default_agent: structured-assistant
`)

	cfg := Config{ConfigPath: filepath.Join(workspace, "xira.yaml"), StateDir: stateRoot}
	manager, err := NewSessionManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SessionManager = manager
	rt, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Give me the requested structured result.",
		Context: channel.NewInboundContext("test", "live-json-format-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != "completed" || resp.ModelPolicy.Format != "json" {
		t.Fatalf("status/model policy = %q/%+v, want completed JSON run", resp.Status, resp.ModelPolicy)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(resp.FinalResponse), &object); err != nil {
		t.Fatalf("final response = %q, want JSON object: %v", resp.FinalResponse, err)
	}
	if len(object) != 2 || object["status"] != "ok" || object["count"] != float64(2) {
		t.Fatalf("final object = %#v, want exactly status=ok and count=2", object)
	}
	if _, ok := findEvent(resp.Events, "assistant.final"); !ok {
		t.Fatalf("completed JSON run missing assistant.final: %v", eventKinds(resp.Events))
	}
}
