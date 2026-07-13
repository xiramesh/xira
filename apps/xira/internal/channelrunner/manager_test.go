package channelrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelrunner/ingest"
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
	cfg := runtime.Config{ConfigPath: configPath}
	manager, err := runtime.NewSessionManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SessionManager = manager
	rt, err := runtime.NewService(cfg)
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

// TestManagerSetOwnerResolverForwarding covers SetOwnerResolver's forwarding
// to all registered runners (feishu/ilink/websocket), plus the nil-receiver
// guard. Previously 0% — this is the #121 injection path.
func TestManagerSetOwnerResolverForwarding(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ilink-pair
    channel: ilink
    enabled: true
    allow_runtime_pairing: true
    state_dir: .xira/ilink-pair
    default_agent: xira-assistant
  - id: ws-default
    channel: websocket
    enabled: true
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))
	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	manager, err := NewManager(rt)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Forward a stub resolver to all runners — must not panic.
	manager.SetOwnerResolver(stubManagerOwnerResolver{})
	// Nil receiver guard: must not panic on nil Manager.
	var nilMgr *Manager
	nilMgr.SetOwnerResolver(nil)
	nilMgr.SetHITLResolver(nil)
	nilMgr.SetTextHITLResolver(nil)
	nilMgr.SetAsyncExactHITLResolver(nil)
	// WSRunner must find the websocket runner.
	if manager.WSRunner() == nil {
		t.Error("WSRunner() returned nil, want websocket runner registered")
	}
}

func TestManagerSetIngestForwardsSameInstanceToEveryRunner(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: feishu-default
    channel: feishu
    enabled: true
    app_id: cli_test
    app_secret: test-secret
    default_agent: xira-assistant
  - id: ilink-default
    channel: ilink
    enabled: true
    allow_runtime_pairing: true
    state_dir: .xira/ilink-default
    default_agent: xira-assistant
  - id: ws-default
    channel: websocket
    enabled: true
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))
	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	manager, err := NewManager(rt)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if manager.Count() != 3 {
		t.Fatalf("runner count = %d, want feishu + ilink + websocket", manager.Count())
	}
	shared := ingest.New(rt.SessionManager(), rt)

	manager.SetIngest(shared)

	want := reflect.ValueOf(shared).Pointer()
	for _, runner := range manager.runners {
		field := reflect.ValueOf(runner).Elem().FieldByName("ingest")
		if !field.IsValid() || field.Kind() != reflect.Pointer {
			t.Fatalf("runner %s has no ingest dependency", runner.Channel())
		}
		if got := field.Pointer(); got != want {
			t.Errorf("runner %s received ingest %#x, want shared %#x", runner.Channel(), got, want)
		}
	}
	var nilManager *Manager
	nilManager.SetIngest(shared)
}

// stubManagerOwnerResolver is a no-op OwnerResolver for Manager forwarding tests.
type stubManagerOwnerResolver struct{}

func (stubManagerOwnerResolver) IsOwner(_ context.Context, _, _ string) bool { return false }

// TestManagerPairingErrors covers the pairing-related methods' error paths
// (unknown entrypoint, empty entrypoint id). Previously 0%.
func TestManagerPairingErrors(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ilink-pair
    channel: ilink
    enabled: true
    allow_runtime_pairing: true
    state_dir: .xira/ilink-pair
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))
	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	manager, err := NewManager(rt)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx := context.Background()
	// Empty entrypoint id → error.
	if _, err := manager.CreatePairing(ctx, "  "); err == nil {
		t.Error("CreatePairing with empty entrypoint id should error")
	}
	if _, err := manager.GetPairing("  ", "p1"); err == nil {
		t.Error("GetPairing with empty entrypoint id should error")
	}
	if _, err := manager.ListAccounts("  "); err == nil {
		t.Error("ListAccounts with empty entrypoint id should error")
	}
	if err := manager.DeleteAccount(ctx, "  ", "a1"); err == nil {
		t.Error("DeleteAccount with empty entrypoint id should error")
	}
	// Unknown entrypoint (not running) → error.
	if _, err := manager.CreatePairing(ctx, "nonexistent-ep"); err == nil {
		t.Error("CreatePairing with unknown entrypoint should error")
	}
	if _, err := manager.GetPairing("nonexistent-ep", "p1"); err == nil {
		t.Error("GetPairing with unknown entrypoint should error")
	}
	if _, err := manager.ListAccounts("nonexistent-ep"); err == nil {
		t.Error("ListAccounts with unknown entrypoint should error")
	}
	if err := manager.DeleteAccount(ctx, "nonexistent-ep", "a1"); err == nil {
		t.Error("DeleteAccount with unknown entrypoint should error")
	}
}

// TestManagerStartStop covers Start + Stop lifecycle on a manager with runners.
// Previously 0%.
func TestManagerStartStop(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ws-default
    channel: websocket
    enabled: true
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))
	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	manager, err := NewManager(rt)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ctx := context.Background()
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if manager.Count() == 0 {
		t.Fatal("after Start, runner count should be > 0")
	}
	// Start again should be idempotent-ish (already started runners skip).
	if err := manager.Start(ctx); err != nil {
		t.Logf("second Start returned: %v (acceptable)", err)
	}
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Stop on nil manager must not panic.
	var nilMgr *Manager
	if err := nilMgr.Stop(ctx); err != nil {
		t.Errorf("nil Manager.Stop should be no-op, got: %v", err)
	}
}

// TestNewManagerNilRuntime covers the nil-rt guard (returns empty Manager).
func TestNewManagerNilRuntime(t *testing.T) {
	m, err := NewManager(nil)
	if err != nil {
		t.Fatalf("NewManager(nil): %v", err)
	}
	if m == nil || m.Count() != 0 {
		t.Errorf("NewManager(nil) should return empty Manager, got count=%d", m.Count())
	}
}

// TestNewManagerUnsupportedChannel covers the default branch (unknown channel).
func TestNewManagerUnsupportedChannel(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: bad-channel
    channel: telegram
    enabled: true
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))
	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	if _, err := NewManager(rt); err == nil {
		t.Fatal("NewManager with unsupported channel should error")
	}
}

// TestManagerSetHITLResolverForwarding covers SetHITLResolver forwarding to
// all runners (previously 28.6% — only the nil guard was covered).
func TestManagerSetHITLResolverForwarding(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ws-default
    channel: websocket
    enabled: true
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))
	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	manager, err := NewManager(rt)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Forward nil resolver — must not panic on websocket runner.
	manager.SetHITLResolver(nil)
	// WSRunner nil-receiver guard.
	var nilMgr *Manager
	if nilMgr.WSRunner() != nil {
		t.Error("nil Manager.WSRunner() should return nil")
	}
}

// errRunner is a Runner whose Stop returns an error, to cover stopRunners'
// firstErr collection branch.
type errRunner struct{}

func (errRunner) ID() string                    { return "err-runner" }
func (errRunner) Channel() string               { return "test" }
func (errRunner) Start(_ context.Context) error { return nil }
func (errRunner) Stop(_ context.Context) error  { return fmt.Errorf("stop failed") }
func (errRunner) SetIngest(*ingest.Ingest)      {}
func (errRunner) Emit(_ context.Context, _ channel.OutboundEnvelope) error {
	return nil
}
func (errRunner) Capabilities() channel.CapabilitySet { return channel.CapabilitySet{} }

// TestStopRunnersCollectsFirstError covers stopRunners' error aggregation:
// when a runner's Stop fails, the first error is returned but remaining
// runners are still stopped.
func TestStopRunnersCollectsFirstError(t *testing.T) {
	ctx := context.Background()
	// Two runners: first errors, second is a real ws runner (Stop ok).
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ws-default
    channel: websocket
    enabled: true
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))
	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	mgr, err := NewManager(rt)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	runners := append([]Runner{errRunner{}}, mgr.runners...)
	err = stopRunners(ctx, runners)
	if err == nil {
		t.Fatal("stopRunners with an erroring runner should return error")
	}
	if !strings.Contains(err.Error(), "stop failed") {
		t.Errorf("stopRunners error = %q, want 'stop failed'", err.Error())
	}
}

// TestManagerPairingSuccess covers pairingController's success branch
// (ListAccounts on a real pairing-enabled ilink runner). Previously the
// success path was uncovered (61.5%).
func TestManagerPairingSuccess(t *testing.T) {
	instance := t.TempDir()
	writeFile(t, filepath.Join(instance, "xira.yaml"), `workspace: workspace
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(instance, "workspace", "entrypoints.yaml"), `entrypoints:
  - id: ilink-pair
    channel: ilink
    enabled: true
    allow_runtime_pairing: true
    state_dir: .xira/ilink-pair
    default_agent: xira-assistant
`)
	writeMinimalAgent(t, filepath.Join(instance, "workspace", "agents", "xira-assistant"))
	rt := newManagerTestRuntime(t, filepath.Join(instance, "xira.yaml"))
	manager, err := NewManager(rt)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// ListAccounts on a pairing runner → empty list (no accounts paired yet),
	// but no error. This covers the success branch of pairingController.
	accounts, err := manager.ListAccounts("ilink-pair")
	if err != nil {
		t.Fatalf("ListAccounts on pairing runner: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("expected 0 accounts on fresh pairing runner, got %d", len(accounts))
	}
}

// TestManagerStartNilAndError covers Start's nil-receiver guard + the
// runner-error rollback branch (stopRunners on partially-started runners).
func TestManagerStartNilAndError(t *testing.T) {
	// nil Manager.Start → no-op, no panic.
	var nilMgr *Manager
	if err := nilMgr.Start(context.Background()); err != nil {
		t.Errorf("nil Manager.Start should be no-op, got: %v", err)
	}
	if got := nilMgr.Count(); got != 0 {
		t.Errorf("nil Manager.Count() = %d, want 0", got)
	}
	// Runner that errors on Start → Start rolls back (calls stopRunners on
	// already-started runners) and returns the error.
	mgr := &Manager{runners: []Runner{errStartRunner{}, errRunner{}}}
	if err := mgr.Start(context.Background()); err == nil {
		t.Error("Start with an erroring runner should return error")
	}
}

// errStartRunner is a Runner whose Start returns an error.
type errStartRunner struct{}

func (errStartRunner) ID() string                    { return "err-start" }
func (errStartRunner) Channel() string               { return "test" }
func (errStartRunner) Start(_ context.Context) error { return fmt.Errorf("start failed") }
func (errStartRunner) Stop(_ context.Context) error  { return nil }
func (errStartRunner) SetIngest(*ingest.Ingest)      {}
func (errStartRunner) Emit(_ context.Context, _ channel.OutboundEnvelope) error {
	return nil
}
func (errStartRunner) Capabilities() channel.CapabilitySet { return channel.CapabilitySet{} }
