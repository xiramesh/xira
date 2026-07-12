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

// TestRealDeepSeekUpdateProfileTool 是 #127 的 live 测试（双门控，§5.3）。
//
// 验证端到端链路：user.md 注入 prompt → LLM 理解该调 update_profile → 产出合法
// function call → handler 执行 → user.md 更新。这是单元测试覆盖不到的「LLM 真的会调」。
//
// prompt 明确引导 LLM 调 update_profile（避免 LLM 自由发挥导致 flaky）。
func TestRealDeepSeekUpdateProfileTool(t *testing.T) {
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek profile smoke tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek profile smoke tests")
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

	// PROFILE.md 显式声明 update_profile 在 permissions.tools（registry 路径依赖它）。
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), `---
id: xira-assistant
name: Xira Assistant
version: 0.1.1
description: Live profile smoke assistant.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
permissions:
  tools:
    - update_profile
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

When the user tells you their name or a preference, record it using the update_profile tool.
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

	// 用 NewService 而非 newTestService——后者注入 fakeDeepSeekClient。
	// live 测试要真 client（NewService 在 DeepSeekClient=nil 时用 deepseek.New()）。
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

	sender := "ou_live_profile_user"
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "你好！我叫大明，以后请这样称呼我。请用 update_profile 工具记住我的名字。",
		Context: channel.NewInboundContext("test", sender, nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	t.Logf("live profile run: status=%q final=%q tool_calls=%d", resp.Status, previewText(resp.FinalResponse, 80), len(resp.ToolCalls))

	// 核心断言：user.md 被更新（含"大明"）。
	userPath := rtools.UserProfilePath(rt.stateDir, sender)
	data, readErr := os.ReadFile(userPath)
	if readErr != nil {
		t.Fatalf("user.md not created at %s after update_profile run: %v (status=%q)", userPath, readErr, resp.Status)
	}
	body := string(data)
	if !strings.Contains(body, "大明") {
		t.Errorf("user.md does not contain 大明 after run:\n%s", body)
	}
	t.Logf("user.md content after run:\n%s", body)
}
