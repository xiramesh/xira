package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/xiramesh/xira/internal/chatkey"
)

// profile.go 实现 #127 per-sender 用户档案（user.md）的读写。
//
// 设计：update_profile 工具（LLM 主动调）写 + runtime 读注入（instructionTextForRun）。
// 读写在同一维度（runtime 层专用路径），独立于 #126 的 data_isolation 开关——
// 每个 sender 无条件有 user.md。
//
// 格式：简单 markdown，按 ## section 组织（身份/偏好/背景...）。
// update_profile 做 section 级增量更新（替换某 section，不碰其他），避免全量覆盖写偏。

// userProfile 是读入的 user.md 内容（#127）。
type userProfile struct {
	Content string // 原始 markdown 内容
	Exists  bool   // 文件是否存在
}

// loadUserProfile 读 user.md。文件不存在返回空 profile（Exists=false，不报错）。
func loadUserProfile(path string) (userProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return userProfile{}, nil
		}
		return userProfile{}, err
	}
	return userProfile{Content: string(data), Exists: true}, nil
}

// sectionHeaderRe 匹配 markdown 二级标题（## xxx），用于定位 section。
var sectionHeaderRe = regexp.MustCompile(`(?m)^##\s+(.+?)\s*$`)

// profileMu 保护 updateProfileSection 的读-改-写原子性（PR #147 review blocker 2）。
// per-path 锁：同一 sender（同一 user.md 路径）的并发更新串行，不同 sender 不互斥。
var profileMu = profileMutex{}

type profileMutex struct {
	mu sync.Mutex // 保护 paths map 本身
	m  map[string]*sync.Mutex
}

func (p *profileMutex) lockFor(path string) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]*sync.Mutex{}
	}
	if p.m[path] == nil {
		p.m[path] = &sync.Mutex{}
	}
	return p.m[path]
}

// updateProfileSection 更新 user.md 的指定 section（原子，PR #147 review blocker 2）：
//   - section 存在 → 替换其内容（## 标题保留，标题下的行换成 content）
//   - section 不存在 → 追加新 section
//   - 文件不存在 → 创建（含父目录，0o600）
//
// 读-改-写整段持 per-path 锁，保证同一 sender 的并发更新不丢（silent data loss 防御）。
// content 是 section 标题下的正文（不含 ## 行）。
func updateProfileSection(path, section, content string) error {
	mu := profileMu.lockFor(path)
	mu.Lock()
	defer mu.Unlock()

	existing, err := loadUserProfile(path)
	if err != nil {
		return err
	}
	var updated string
	if existing.Exists {
		updated = replaceOrAppendSection(existing.Content, section, content)
	} else {
		updated = newProfileWithSection(section, content)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write user.md: %w", err)
	}
	return nil
}

// replaceOrAppendSection 在已有 user.md 内容里替换或追加一个 section。
func replaceOrAppendSection(body, section, content string) string {
	// 找 section 标题的位置
	loc := findSectionLocation(body, section)
	if loc == nil {
		// section 不存在 → 追加
		return appendSection(body, section, content)
	}
	// section 存在 → 替换标题到下一个 section（或文末）之间的内容
	headerStart := loc[0]
	contentEnd := findSectionEnd(body, loc[1])
	before := body[:headerStart]
	after := body[contentEnd:]
	newSection := formatSection(section, content)
	return before + newSection + after
}

// findSectionLocation 找 `## {section}` 标题行在 body 里的 [start, end]（标题行整体）。
// 返回 nil 表示没找到。
func findSectionLocation(body, section string) []int {
	target := "## " + section
	for _, m := range sectionHeaderRe.FindAllStringSubmatchIndex(body, -1) {
		// m = [matchStart, matchEnd, groupStart, groupEnd]
		if m[2] >= 0 && strings.TrimSpace(body[m[2]:m[3]]) == section {
			return []int{m[0], m[1]}
		}
	}
	_ = target // (target 留作文档参考；匹配用 group)
	return nil
}

// findSectionEnd 找从 pos 开始，下一个 ## 标题的位置（或文末）。
func findSectionEnd(body string, afterHeaderEnd int) int {
	rest := body[afterHeaderEnd:]
	nextHeader := sectionHeaderRe.FindStringIndex(rest)
	if nextHeader == nil {
		return len(body) // 没有下一个 section → 到文末
	}
	return afterHeaderEnd + nextHeader[0]
}

// formatSection 生成一个 section 的 markdown（## 标题 + 内容）。
func formatSection(section, content string) string {
	c := strings.TrimRight(content, "\n")
	return "## " + section + "\n" + c + "\n"
}

// appendSection 在 body 末尾追加一个新 section。
func appendSection(body, section, content string) string {
	body = strings.TrimRight(body, "\n")
	return body + "\n\n" + formatSection(section, content)
}

// newProfileWithSection 生成首次创建的 user.md（含一个 section）。
func newProfileWithSection(section, content string) string {
	return "# 用户档案\n\n" + formatSection(section, content)
}

// UpdateProfileTool 让 LLM 更新当前 sender 的 user.md（#127）。
//
// LLM 在对话中判断「该记点什么了」（如用户说"叫我大明"）就调本工具。
// 入参是 section + content（section 级增量更新），不是全量覆盖——agent 只提供
// 「我学到的内容」，路径/格式/原子性由 runtime 保证（避免写偏）。
//
// 独立于 #126 的 data_isolation：用 chatkey.SenderIDFromContext（无门控），
// 每个 sender 无条件有 user.md。
//
// 安全（PR #147 review）：user.md 落在 stateDir（不是 workspace），通用工具
// （fs/command/shell）只有 workspaceRoot，根本看不到 user.md——无需给每扇门装锁。
type UpdateProfileTool struct {
	stateDir string
}

func NewUpdateProfileTool(stateDir string) *UpdateProfileTool {
	return &UpdateProfileTool{stateDir: strings.TrimSpace(stateDir)}
}

func (t *UpdateProfileTool) Name() string { return "update_profile" }

func (t *UpdateProfileTool) Description() string {
	return "Update the current user's profile (user.md) with learned information about them — " +
		"their name, preferences, background, or anything worth remembering for future conversations. " +
		"Provide a section name (e.g. \"身份\", \"偏好\", \"背景\") and the content to store under it. " +
		"Call this when the user shares personal info or preferences you should remember."
}

func (t *UpdateProfileTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"section", "content"},
		"properties": map[string]any{
			"section": map[string]any{
				"type":        "string",
				"description": "Profile section to update (e.g. \"身份\" for identity, \"偏好\" for preferences, \"背景\" for background). Existing section is replaced; new section is appended.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Markdown content for the section (without the ## header — that's added automatically).",
			},
		},
	}
}

func (t *UpdateProfileTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	senderID, _ := chatkey.SenderIDFromContext(ctx)
	senderID = strings.TrimSpace(senderID)
	if senderID == "" {
		return nil, fmt.Errorf("update_profile requires a sender (no chatKey in context)")
	}
	section, ok := args["section"].(string)
	if !ok || strings.TrimSpace(section) == "" {
		return nil, fmt.Errorf("section is required")
	}
	content, ok := args["content"].(string)
	if !ok {
		return nil, fmt.Errorf("content is required")
	}
	path := UserProfilePath(t.stateDir, senderID)
	if path == "" {
		return nil, fmt.Errorf("cannot resolve user.md path for sender %q", senderID)
	}
	if err := updateProfileSection(path, section, content); err != nil {
		return nil, err
	}
	// PR #147 review：不返回绝对路径——防止模型拿路径喂 command.run/shell.run
	// 读他人 user.md（command/shell 不是 OS 沙箱，能读任意路径）。
	return map[string]any{"updated": true, "section": section}, nil
}
