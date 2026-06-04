package channelrunner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ai-daming/flowdeck/internal/runtime"
)

func TestManagerIgnoresDisabledEntrypoints(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "flowdeck.yaml"), `workspace: workspace
default_agent: flowdeck-assistant
run_root: .flowdeck/runs
entrypoints: workspace/entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: feishu-default
    channel: feishu
    enabled: false
    app_id: cli_xxx
    app_secret_env: MISSING_FEISHU_SECRET
    default_agent: flowdeck-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "flowdeck-assistant"))

	rt, err := runtime.NewService(runtime.Config{ConfigPath: filepath.Join(instance, "flowdeck.yaml"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(rt)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Count() != 0 {
		t.Fatalf("runner count = %d, want 0", manager.Count())
	}
}

func TestManagerRequiresFeishuCredentialsForEnabledEntrypoint(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "flowdeck.yaml"), `workspace: workspace
default_agent: flowdeck-assistant
run_root: .flowdeck/runs
entrypoints: workspace/entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: feishu-default
    channel: feishu
    enabled: true
    default_agent: flowdeck-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "flowdeck-assistant"))

	rt, err := runtime.NewService(runtime.Config{ConfigPath: filepath.Join(instance, "flowdeck.yaml"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewManager(rt); err == nil {
		t.Fatal("expected missing credential error")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func writeMinimalAgent(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "PROFILE.md"), `---
id: flowdeck-assistant
name: FlowDeck Assistant
version: 0.1.0
description: Test assistant.
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
  temperature: 0.2
session:
  dimensions:
    - chat
    - sender
verification:
  default_checks:
    - final_response_non_empty
---
# Contract

Keep responses short.
`)
	writeFile(t, filepath.Join(dir, "SOUL.md"), `# Soul
`)
}
