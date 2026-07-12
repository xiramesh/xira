package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
	rtools "github.com/xiramesh/xira/internal/tools"
)

func TestRealDeepSeekFinishSilentAfterAgentMemory(t *testing.T) {
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek intentional silence smoke tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek intentional silence smoke tests")
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
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.0
description: Live intentional silence smoke assistant.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
permissions:
  tools:
    - update_memory
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

When asked to remember your own durable procedure without replying, call update_memory exactly once with scope=agent. After it succeeds, call finish_silent exactly once, then return no text at all.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), "# Soul\n\nDirect.\n")
	writeFile(t, filepath.Join(workspace, "xira.yaml"), `workspace: .
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(workspace, "entrypoints.yaml"), `entrypoints:
  - id: test-default
    channel: test
    default_agent: xira-assistant
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
		Message: "把『发布前检查 CI』记成你自己的长期工作流程。处理完成后不要回复我，也不要通知 owner。",
		Context: channel.NewInboundContext("test", "ou_live_silent_sender", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if resp.Status != "completed" || resp.FinalResponse != "" || resp.VerificationResult.Status != "passed" {
		t.Fatalf("live silence response status=%q final=%q verification=%+v", resp.Status, resp.FinalResponse, resp.VerificationResult)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Name != "update_memory" || resp.ToolCalls[0].Input["scope"] != "agent" || resp.ToolCalls[1].Name != finishSilentToolName {
		t.Fatalf("live silence tool calls = %+v", resp.ToolCalls)
	}
	entries, loadErr := rtools.LoadMemories(rtools.AgentMemoryPath(rt.stateDir, "xira-assistant"))
	if loadErr != nil || len(entries) == 0 {
		t.Fatalf("live Agent memory missing: %+v err=%v", entries, loadErr)
	}
	found := false
	for _, entry := range entries {
		if strings.Contains(entry.Content, "CI") || strings.Contains(entry.Content, "发布") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("live Agent memory missing CI/release procedure: %+v", entries)
	}
}
