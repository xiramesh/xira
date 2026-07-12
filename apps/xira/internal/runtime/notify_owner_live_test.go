package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

// TestRealDeepSeekNotifyOwnerAndStaySilent verifies the actual model/tool loop,
// not a mocked tool choice. Delivery uses a recording emitter because CI has
// no Feishu tenant credentials; Feishu request shape is covered separately by
// the adapter tests.
func TestRealDeepSeekNotifyOwnerAndStaySilent(t *testing.T) {
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek notify_owner smoke tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek notify_owner smoke tests")
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
version: 0.1.1
description: Live owner notification smoke assistant.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

When explicitly asked to notify the owner privately, call notify_owner exactly once. If it returns sent, do not produce any public response text.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), "# Soul\n\nYou are the owner's AI intern. Never impersonate the owner.\n")
	writeFile(t, filepath.Join(workspace, "xira.yaml"), `workspace: .
default_agent: xira-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(workspace, "entrypoints.yaml"), `entrypoints:
  - id: test-owner
    channel: test
    default_agent: xira-assistant
`)
	writeFile(t, filepath.Join(stateRoot, ownerBindingsFilename), `{
  "bindings": [
    {
      "entrypoint_id": "test-owner",
      "owner_sender_id": "owner-live",
      "owner_sender_id_type": "test_user_id",
      "bound_at": "2026-07-12T00:00:00Z"
    }
  ]
}
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
	emitter := &recordingEmitter{}
	rt.SetOutboundEmitter(emitter)

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		EntrypointID: "test-owner",
		Message:      "Privately notify the owner with exactly this message: Live notify_owner smoke passed. After the tool reports sent, return no public text at all.",
		Context: channel.InboundContext{
			Channel:      "test",
			EntrypointID: "test-owner",
			ChatID:       "group-live",
			ChatType:     "group",
			SenderID:     "coworker-live",
			AddressedTo:  []channel.AddressTarget{channel.AddressTargetOwner},
		},
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != "completed" || resp.FinalResponse != "" {
		t.Fatalf("live notify response status=%q final=%q verification=%+v", resp.Status, resp.FinalResponse, resp.VerificationResult)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != notifyOwnerToolName || resp.ToolCalls[0].Output["status"] != "sent" {
		t.Fatalf("live notify tool calls = %+v", resp.ToolCalls)
	}
	calls := emitter.emitted()
	if len(calls) != 1 || calls[0].Recipient == nil || calls[0].Recipient.ID != "owner-live" || calls[0].Data["content"] != "Live notify_owner smoke passed." {
		t.Fatalf("live notify envelopes = %+v", calls)
	}
}
