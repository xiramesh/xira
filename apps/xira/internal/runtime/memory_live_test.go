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

// TestRealDeepSeekUpdateMemoryTool 是 #128 的 live 测试（双门控，§5.3）。
// 验证 LLM 真调 update_memory（端到端：注入 prompt → LLM 理解 → 产出 function call → handler 写 memory.jsonl）。
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

When the user mentions a plan or event, record it using the update_memory tool.
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
	rt, err := NewService(Config{
		ConfigPath: filepath.Join(workspace, "xira.yaml"),
		StateDir:   stateRoot,
	})
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
}
