package channelrunner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xiramesh/xira/internal/runtime"
)

func TestManagerIgnoresDisabledEntrypoints(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: feishu-default
    channel: feishu
    enabled: false
    app_id: cli_xxx
    app_secret_env: MISSING_FEISHU_SECRET
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))

	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
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
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: feishu-default
    channel: feishu
    enabled: true
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))

	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	if _, err := NewManager(rt); err == nil {
		t.Fatal("expected missing credential error")
	}
}

func TestManagerRequiresIlinkTokenForEnabledEntrypoint(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ilink-default
    channel: ilink
    enabled: true
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))

	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	if _, err := NewManager(rt); err == nil {
		t.Fatal("expected missing token error")
	}
}

func TestManagerAllowsRuntimeIlinkPairingWithoutToken(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ilink-default
    channel: ilink
    enabled: true
    allow_runtime_pairing: true
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))

	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	manager, err := NewManager(rt)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Count() != 1 {
		t.Fatalf("runner count = %d, want 1", manager.Count())
	}
}

func TestManagerRegistersIlinkEntrypoint(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ilink-default
    channel: ilink
    enabled: true
    token_env: TEST_ILINK_TOKEN
    state_dir: .xira/ilink/test
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))
	t.Setenv("TEST_ILINK_TOKEN", "bot-token")

	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	manager, err := NewManager(rt)
	if err != nil {
		t.Fatal(err)
	}
	if manager.Count() != 1 {
		t.Fatalf("runner count = %d, want 1", manager.Count())
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

func newManagerTestRuntime(t *testing.T, configPath string) *runtime.Service {
	t.Helper()
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	rt, err := runtime.NewService(runtime.Config{ConfigPath: configPath})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func writeMinimalAgent(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
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
