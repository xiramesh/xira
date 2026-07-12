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

// TestRealDeepSeekUpdateMemoryTool is the #128/#159 live test. It verifies
// that the real model distinguishes memory about the current sender from
// memory the Agent itself must retain across senders.
func TestRealDeepSeekUpdateMemoryTool(t *testing.T) {
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek memory smoke tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek memory smoke tests")
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
description: Live memory smoke assistant.
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

When the user asks you to remember a fact about that user, call update_memory exactly once with scope=sender.
When the user asks you to remember your own durable procedure or follow-up across users, call update_memory exactly once with scope=agent.
After the tool succeeds, reply with a short confirmation.
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

	// 用 NewService（真 DeepSeek client，不走 fake）。
	cfg := Config{
		ConfigPath: filepath.Join(workspace, "xira.yaml"),
		StateDir:   stateRoot,
	}
	manager, err := NewSessionManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SessionManager = manager
	rt, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}

	sender := "ou_live_memory_user"
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "你好！我下周三要去上海出差，请用 update_memory 工具帮我记一下。",
		Context: channel.NewInboundContext("test", sender, nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "update_memory" || resp.ToolCalls[0].Input["scope"] != "sender" {
		t.Fatalf("sender run tool calls = %+v, want one update_memory scope=sender", resp.ToolCalls)
	}
	t.Logf("live memory run: status=%q final=%q tool_calls=%d", resp.Status, previewText(resp.FinalResponse, 80), len(resp.ToolCalls))

	// 核心断言：memory.jsonl 被写入（含"上海"或"出差"）。
	memPath := rtools.MemoryPath(rt.stateDir, sender)
	entries, readErr := rtools.LoadMemories(memPath)
	if readErr != nil || len(entries) == 0 {
		t.Fatalf("memory.jsonl not written or empty at %s: %v (status=%q)", memPath, readErr, resp.Status)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Content, "上海") || strings.Contains(e.Content, "出差") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("memory does not contain 上海/出差. Entries: %+v", entries)
	}
	t.Logf("memory entries after run: %+v", entries)

	secondSender := "ou_live_memory_colleague"
	agentResp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "请把『每次发布前先检查 CI』记成你自己跨用户都要遵循的长期工作经验；必须调用 update_memory，scope 使用 agent。",
		Context: channel.NewInboundContext("test", secondSender, nil),
	})
	if err != nil {
		t.Fatalf("agent-scope RunAgent() error = %v", err)
	}
	if len(agentResp.ToolCalls) != 1 || agentResp.ToolCalls[0].Name != "update_memory" || agentResp.ToolCalls[0].Input["scope"] != "agent" {
		t.Fatalf("agent run tool calls = %+v, want one update_memory scope=agent", agentResp.ToolCalls)
	}
	agentEntries, readErr := rtools.LoadMemories(rtools.AgentMemoryPath(rt.stateDir, "xira-assistant"))
	if readErr != nil || len(agentEntries) == 0 {
		t.Fatalf("agent memory not written: entries=%+v err=%v", agentEntries, readErr)
	}
	found = false
	for _, entry := range agentEntries {
		if strings.Contains(entry.Content, "CI") || strings.Contains(entry.Content, "发布") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("agent memory does not contain CI/release procedure: %+v", agentEntries)
	}
	if secondEntries, secondErr := rtools.LoadMemories(rtools.MemoryPath(rt.stateDir, secondSender)); secondErr != nil || len(secondEntries) != 0 {
		t.Fatalf("agent-scope write polluted second sender memory: %+v err=%v", secondEntries, secondErr)
	}

	// Restart against the same state root and prove the persisted Agent memory
	// is loaded into the Agent's instruction independently of either sender.
	restartCfg := Config{ConfigPath: filepath.Join(workspace, "xira.yaml"), StateDir: stateRoot}
	restartManager, err := NewSessionManager(restartCfg)
	if err != nil {
		t.Fatalf("restart session manager: %v", err)
	}
	restartCfg.SessionManager = restartManager
	restarted, err := NewService(restartCfg)
	if err != nil {
		t.Fatalf("restart service: %v", err)
	}
	profile, ok := restarted.agents.Get("xira-assistant")
	if !ok {
		t.Fatal("restarted service missing xira-assistant")
	}
	instruction, _, err := restarted.instructionTextForRun(profile, channel.NewInboundContext("test", "ou_third_sender", nil))
	if err != nil {
		t.Fatalf("restart instruction: %v", err)
	}
	if !strings.Contains(instruction, "# Agent Memory") || (!strings.Contains(instruction, "CI") && !strings.Contains(instruction, "发布")) {
		t.Fatalf("restarted instruction missing persisted Agent memory:\n%s", instruction)
	}
}
