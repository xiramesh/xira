package runtime

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
	rtools "github.com/xiramesh/xira/internal/tools"
)

func TestADKUpdateMemoryAgentScopeUsesRuntimeBoundAgentIdentity(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.0
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
  stream: false
permissions:
  tools:
    - update_memory
verification:
  default_checks:
    - final_response_non_empty
---
# Contract

Use memory tools as requested.
`)
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), "# Soul\n\nDirect.\n")
	writeFile(t, filepath.Join(workspace, "xira.yaml"), "workspace: .\ndefault_agent: xira-assistant\n")

	modelCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		if modelCalls == 1 {
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("agent-memory-1", "update_memory", map[string]any{
				"scope": "agent", "key": "release", "content": "check CI before release",
			})), nil
		}
		return deepSeekHTTPResponse(deepSeekTextResponse("Remembered for myself.")), nil
	})}
	stateDir := t.TempDir()
	rt := newTestService(t, Config{
		ConfigPath: filepath.Join(workspace, "xira.yaml"),
		StateDir:   stateDir,
		DeepSeekClient: deepseek.New(
			deepseek.WithBaseURLForTest("http://deepseek.test"),
			deepseek.WithAPIKey("test-key"),
			deepseek.WithHTTPClient(client),
		),
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "remember your release procedure",
		Context: channel.NewInboundContext("test", "ou_colleague", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if resp.Status != "completed" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Output["scope"] != "agent" {
		t.Fatalf("response = %+v", resp)
	}
	agentEntries, loadErr := rtools.LoadMemories(rtools.AgentMemoryPath(stateDir, "xira-assistant"))
	if loadErr != nil || len(agentEntries) != 1 || agentEntries[0].Content != "check CI before release" {
		t.Fatalf("agent entries = %+v, err=%v", agentEntries, loadErr)
	}
	senderEntries, loadErr := rtools.LoadMemories(rtools.MemoryPath(stateDir, "ou_colleague"))
	if loadErr != nil || len(senderEntries) != 0 {
		t.Fatalf("sender memory polluted: %+v, err=%v", senderEntries, loadErr)
	}
}
