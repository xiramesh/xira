package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/xiramesh/xira/internal/chatkey"
	"github.com/xiramesh/xira/internal/session"
)

// memory.go 实现 #128 per-sender 交互记忆（memory.jsonl）。
//
// 每条记忆是一个 JSON 对象（jsonl 一行），带 key/content/时间/状态等结构化属性。
// 两个工具：update_memory（upsert by key）+ forget_memory（软删除 by key）。
// runtime 渲染注入：读 active 且未过期的行，渲染成可读文本注入 prompt。
//
// 安全模型（同 #127 弱便签）：路径落 stateDir，不返回路径给模型，动态定界符防注入，
// per-path mutex 防并发丢更新，非强私密（command/shell 能读任意路径，强私有归 #148）。

const (
	memoriesSegment       = "memories"
	memoryFilename        = "memory.jsonl"
	memoryStatusActive    = "active"
	memoryStatusForgotten = "forgotten"
)

// MemoryPath 算出 sender 的 memory.jsonl 路径（#128）。
// baseDir 是 stateDir（不是 workspace）。落 baseDir/memories/sender_{safe}/memory.jsonl。
// senderID 为空 → 返回 ""。
func MemoryPath(baseDir, senderID string) string {
	senderID = strings.TrimSpace(senderID)
	if senderID == "" {
		return ""
	}
	safe := session.SafePathID(senderID)
	return filepath.Join(baseDir, memoriesSegment, "sender_"+safe, memoryFilename)
}

// MemoryEntry 是一条记忆（jsonl 一行）。
type MemoryEntry struct {
	ID      string     `json:"id"`
	Key     string     `json:"key"`
	Content string     `json:"content"`
	Created time.Time  `json:"created"`
	Updated time.Time  `json:"updated"`
	Status  string     `json:"status"`
	Expires *time.Time `json:"expires,omitempty"`
}

// memoryMu 保护 jsonl 的读-改-写原子性（同 profileMutex 模式）。
var memoryMu = memoryMutex{}

type memoryMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (p *memoryMutex) lockFor(path string) *sync.Mutex {
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

// LoadMemories 读 memory.jsonl。文件不存在返回空切片（不报错）。
// 导出供 runtime 包（live 测试）和 tools 包共用。
func LoadMemories(path string) ([]MemoryEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var entries []MemoryEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 允许长行
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e MemoryEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // 跳过坏行（不 crash）
		}
		entries = append(entries, e)
	}
	return entries, scanner.Err()
}

// saveMemories 把 entries 写回 jsonl（全量覆盖，持锁调用）。
func saveMemories(path string, entries []MemoryEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("encode: %w", err)
		}
	}
	return nil
}

// upsertMemory 按 key upsert：同 key 存在→更新 content/updated/expires/status=active；
// 不存在→append 新条目（id=uuid，created=now，status=active）。
func upsertMemory(path, key, content string, expires *time.Time) error {
	mu := memoryMu.lockFor(path)
	mu.Lock()
	defer mu.Unlock()

	entries, err := LoadMemories(path)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i, e := range entries {
		if e.Key == key {
			entries[i].Content = content
			entries[i].Updated = now
			entries[i].Expires = expires
			entries[i].Status = memoryStatusActive // upsert 恢复 forgotten 的
			return saveMemories(path, entries)
		}
	}
	entries = append(entries, MemoryEntry{
		ID:      "mem_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		Key:     key,
		Content: content,
		Created: now,
		Updated: now,
		Status:  memoryStatusActive,
		Expires: expires,
	})
	return saveMemories(path, entries)
}

// forgetMemory 按 key 软删除：status 改 "forgotten"（不物理删）。不存在→no-op。
func forgetMemory(path, key string) error {
	mu := memoryMu.lockFor(path)
	mu.Lock()
	defer mu.Unlock()

	entries, err := LoadMemories(path)
	if err != nil {
		return err
	}
	for i, e := range entries {
		if e.Key == key && e.Status == memoryStatusActive {
			entries[i].Status = memoryStatusForgotten
			entries[i].Updated = time.Now().UTC()
			return saveMemories(path, entries)
		}
	}
	return nil // no-op
}

// ActiveMemories 返回 status=="active" 且（expires 为空或未过期）的记忆。
// 导出供 runtime 包（读注入）调用。
func ActiveMemories(path string) ([]MemoryEntry, error) {
	entries, err := LoadMemories(path)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var active []MemoryEntry
	for _, e := range entries {
		if e.Status != memoryStatusActive {
			continue
		}
		if e.Expires != nil && !e.Expires.After(now) {
			continue // 过期
		}
		active = append(active, e)
	}
	return active, nil
}

// --- 工具 ---

// UpdateMemoryTool 让 LLM 记录交互记忆（#128）。
//
// 记录带时间/事件的事实（"用户问过报销流程""用户下周要出差"），跨会话保留。
// 按 key upsert——同 key 覆盖（更新内容+时间），不重复堆积。
// 可选 expires（ISO8601 日期）——过期后不注入 prompt。
//
// 和 update_profile 的区别：update_profile 存稳定身份偏好（昵称/回复风格），
// update_memory 存事件性事实。description 显式指向 update_profile 做分流。
//
// 安全模型同 #127 弱便签：不返回路径、非强私密、不存敏感数据。
type UpdateMemoryTool struct {
	stateDir string
}

func NewUpdateMemoryTool(stateDir string) *UpdateMemoryTool {
	return &UpdateMemoryTool{stateDir: strings.TrimSpace(stateDir)}
}

func (t *UpdateMemoryTool) Name() string { return "update_memory" }

func (t *UpdateMemoryTool) Description() string {
	return "Record a fact or context about the user that should persist across conversations — " +
		"e.g. events, things they mentioned or asked about, time-sensitive plans. " +
		"Provide a key (short topic identifier like \"出差\", \"报销\", \"宠物\") and the content. " +
		"Same key overwrites the previous entry. Optional expires (ISO8601 date) for time-sensitive memories. " +
		"For stable identity/preferences (nickname, reply style, language), use update_profile instead. " +
		"DO NOT store sensitive data (passwords, contacts, addresses, account IDs)."
}

func (t *UpdateMemoryTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"key", "content"},
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "Short topic identifier for this memory (e.g. \"出差\", \"报销\", \"宠物\"). Same key overwrites.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The fact or context to remember.",
			},
			"expires": map[string]any{
				"type":        "string",
				"description": "Optional ISO8601 date when this memory expires (e.g. \"2026-07-17\"). Expired memories are not injected into prompts.",
			},
		},
	}
}

func (t *UpdateMemoryTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	senderID, _ := chatkey.SenderIDFromContext(ctx)
	senderID = strings.TrimSpace(senderID)
	if senderID == "" {
		return nil, fmt.Errorf("update_memory requires a sender (no chatKey in context)")
	}
	key, ok := args["key"].(string)
	if !ok || strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("key is required")
	}
	content, ok := args["content"].(string)
	if !ok || strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("content is required")
	}
	var expires *time.Time
	if expStr, ok := args["expires"].(string); ok && strings.TrimSpace(expStr) != "" {
		t, err := time.Parse("2006-01-02", strings.TrimSpace(expStr))
		if err == nil {
			expires = &t
		}
	}
	path := MemoryPath(t.stateDir, senderID)
	if path == "" {
		return nil, fmt.Errorf("cannot resolve memory path for sender %q", senderID)
	}
	if err := upsertMemory(path, key, content, expires); err != nil {
		return nil, err
	}
	// 不返回路径（安全：防模型拿路径喂 command.run）
	return map[string]any{"updated": true, "key": key}, nil
}

// ForgetMemoryTool 让 LLM 标记某主题记忆为遗忘（软删除，#128）。
//
// 标记后该记忆不再注入 prompt，但保留在 jsonl 中（审计 + 可恢复）。
// 适合用户说「出差取消了」「这事不用记了」时调用。
type ForgetMemoryTool struct {
	stateDir string
}

func NewForgetMemoryTool(stateDir string) *ForgetMemoryTool {
	return &ForgetMemoryTool{stateDir: strings.TrimSpace(stateDir)}
}

func (t *ForgetMemoryTool) Name() string { return "forget_memory" }

func (t *ForgetMemoryTool) Description() string {
	return "Mark a memory as forgotten — it will no longer be injected into future prompts " +
		"but is retained in the memory file for audit. Use when the user says something is no longer relevant " +
		"(e.g. a plan was cancelled, a question was resolved). Provide the key of the memory to forget."
}

func (t *ForgetMemoryTool) Parameters() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"key"},
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "The key of the memory to forget (e.g. \"出差\").",
			},
		},
	}
}

func (t *ForgetMemoryTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	senderID, _ := chatkey.SenderIDFromContext(ctx)
	senderID = strings.TrimSpace(senderID)
	if senderID == "" {
		return nil, fmt.Errorf("forget_memory requires a sender (no chatKey in context)")
	}
	key, ok := args["key"].(string)
	if !ok || strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("key is required")
	}
	path := MemoryPath(t.stateDir, senderID)
	if path == "" {
		return nil, fmt.Errorf("cannot resolve memory path for sender %q", senderID)
	}
	if err := forgetMemory(path, key); err != nil {
		return nil, err
	}
	return map[string]any{"forgotten": true, "key": key}, nil
}
