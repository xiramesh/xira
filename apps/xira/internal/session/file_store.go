package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const fileStoreVersion = 1

type ConversationMeta struct {
	Version      int           `json:"version"`
	SessionID    string        `json:"session_id"`
	Scope        *SessionScope `json:"scope,omitempty"`
	EntrypointID string        `json:"entrypoint_id,omitempty"`
	Channel      string        `json:"channel,omitempty"`
	Account      string        `json:"account,omitempty"`
	ChatID       string        `json:"chat_id,omitempty"`
	ChatType     string        `json:"chat_type,omitempty"`
	SenderID     string        `json:"sender_id,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
	LastAgentID  string        `json:"last_agent_id,omitempty"`
	LastRunID    string        `json:"last_run_id,omitempty"`
}

type AgentMeta struct {
	Version        int       `json:"version"`
	SessionID      string    `json:"session_id"`
	AgentID        string    `json:"agent_id"`
	AgentSessionID string    `json:"agent_session_id"`
	MessageCount   int       `json:"message_count"`
	Summary        string    `json:"summary,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastRunID      string    `json:"last_run_id,omitempty"`
}

type FileStore struct {
	root string
	mu   sync.Mutex
}

func NewFileStore(root string) (*FileStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("session store root is empty")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create session store %s: %w", root, err)
	}
	return &FileStore{root: root}, nil
}

func (s *FileStore) Root() string {
	if s == nil {
		return ""
	}
	return s.root
}

func (s *FileStore) AppendAgentTurn(input AgentTurnInput, messages []Message) error {
	return s.AppendAgentMessages(input, messages)
}

func (s *FileStore) AppendAgentMessages(input AgentTurnInput, messages []Message) error {
	if s == nil {
		return nil
	}
	messages = compactMessages(messages)
	if len(messages) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	conversationDir := s.conversationDirForInput(input)
	agentDir := s.agentDirForInput(input)
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("create agent session dir: %w", err)
	}
	now := latestMessageTime(messages)
	if now.IsZero() {
		now = time.Now()
	}
	if err := s.writeConversationMeta(input, conversationDir, now); err != nil {
		return err
	}
	if err := appendJSONLines(filepath.Join(agentDir, "messages.jsonl"), messages); err != nil {
		return err
	}
	if err := s.writeAgentMeta(input, agentDir, len(messages), now); err != nil {
		return err
	}
	return nil
}

func (s *FileStore) LoadHistories() (map[string][]Message, map[string]map[string][]Message, error) {
	histories := map[string][]Message{}
	agentHistories := map[string]map[string][]Message{}
	if s == nil {
		return histories, agentHistories, nil
	}
	type orderedMessage struct {
		message Message
		order   int
	}
	combinedBySession := map[string][]orderedMessage{}
	order := 0
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() || path == s.root {
			return nil
		}
		meta, err := readJSONFile[ConversationMeta](filepath.Join(path, "meta.json"))
		if err != nil || strings.TrimSpace(meta.SessionID) == "" {
			return nil
		}
		agentsDir := filepath.Join(path, "agents")
		agentEntries, err := os.ReadDir(agentsDir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read session agents: %w", err)
		}
		for _, agentEntry := range agentEntries {
			if !agentEntry.IsDir() {
				continue
			}
			agentDir := filepath.Join(agentsDir, agentEntry.Name())
			agentMeta, _ := readJSONFile[AgentMeta](filepath.Join(agentDir, "meta.json"))
			agentID := strings.TrimSpace(agentMeta.AgentID)
			if agentID == "" {
				agentID = agentEntry.Name()
			}
			messages, err := readMessages(filepath.Join(agentDir, "messages.jsonl"))
			if err != nil {
				return err
			}
			for i := range messages {
				if messages[i].AgentID == "" {
					messages[i].AgentID = agentID
				}
				combinedBySession[meta.SessionID] = append(combinedBySession[meta.SessionID], orderedMessage{message: messages[i], order: order})
				order++
			}
			if len(messages) > 0 {
				if agentHistories[meta.SessionID] == nil {
					agentHistories[meta.SessionID] = map[string][]Message{}
				}
				agentHistories[meta.SessionID][agentID] = append(agentHistories[meta.SessionID][agentID], messages...)
			}
		}
		return fs.SkipDir
	})
	if os.IsNotExist(err) {
		return histories, agentHistories, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read session store: %w", err)
	}
	for sessionID, combined := range combinedBySession {
		sort.SliceStable(combined, func(i, j int) bool {
			left := combined[i].message.CreatedAt
			right := combined[j].message.CreatedAt
			if left.IsZero() || right.IsZero() || left.Equal(right) {
				return combined[i].order < combined[j].order
			}
			return left.Before(right)
		})
		for _, item := range combined {
			histories[sessionID] = append(histories[sessionID], item.message)
		}
	}
	for _, byAgent := range agentHistories {
		for agentID, messages := range byAgent {
			sort.SliceStable(messages, func(i, j int) bool {
				left := messages[i].CreatedAt
				right := messages[j].CreatedAt
				if left.IsZero() || right.IsZero() || left.Equal(right) {
					return false
				}
				return left.Before(right)
			})
			byAgent[agentID] = messages
		}
	}
	return histories, agentHistories, nil
}

func (s *FileStore) writeConversationMeta(input AgentTurnInput, dir string, now time.Time) error {
	path := filepath.Join(dir, "meta.json")
	meta, _ := readJSONFile[ConversationMeta](path)
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.Version = fileStoreVersion
	meta.SessionID = strings.TrimSpace(input.SessionID)
	meta.Scope = cloneScope(input.Scope)
	meta.EntrypointID = strings.TrimSpace(input.Context.EntrypointID)
	meta.Channel = strings.TrimSpace(input.Context.Channel)
	meta.Account = strings.TrimSpace(input.Context.Account)
	meta.ChatID = strings.TrimSpace(input.Context.ChatID)
	meta.ChatType = strings.TrimSpace(input.Context.ChatType)
	meta.SenderID = strings.TrimSpace(input.Context.SenderID)
	meta.UpdatedAt = now
	meta.LastAgentID = strings.TrimSpace(input.AgentID)
	meta.LastRunID = strings.TrimSpace(input.RunID)
	return writeJSONAtomic(path, meta, 0o600)
}

func (s *FileStore) writeAgentMeta(input AgentTurnInput, dir string, appended int, now time.Time) error {
	path := filepath.Join(dir, "meta.json")
	meta, _ := readJSONFile[AgentMeta](path)
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	meta.Version = fileStoreVersion
	meta.SessionID = strings.TrimSpace(input.SessionID)
	meta.AgentID = strings.TrimSpace(input.AgentID)
	meta.AgentSessionID = strings.TrimSpace(input.AgentSessionID)
	meta.MessageCount += appended
	meta.UpdatedAt = now
	meta.LastRunID = strings.TrimSpace(input.RunID)
	return writeJSONAtomic(path, meta, 0o600)
}

func (s *FileStore) conversationDir(sessionID string) string {
	return filepath.Join(s.root, safePathID(sessionID))
}

func (s *FileStore) agentDir(sessionID, agentID string) string {
	return filepath.Join(s.conversationDir(sessionID), "agents", safePathID(agentID))
}

func (s *FileStore) conversationDirForInput(input AgentTurnInput) string {
	return filepath.Join(
		s.root,
		safePathID(inputChannel(input)),
		safePathID(inputEntrypointID(input)),
		conversationFolderName(input),
	)
}

func (s *FileStore) agentDirForInput(input AgentTurnInput) string {
	return filepath.Join(s.conversationDirForInput(input), "agents", safePathID(input.AgentID))
}

func conversationFolderName(input AgentTurnInput) string {
	parts := []string{}
	if chatID := strings.TrimSpace(input.Context.ChatID); chatID != "" && shouldIncludePathDimension(input, "chat") {
		chatType := strings.TrimSpace(input.Context.ChatType)
		if chatType == "" {
			chatType = "chat"
		}
		parts = append(parts, "chat_"+safePathID(chatType+"_"+chatID))
	} else if value := inputScopeValue(input, "chat"); value != "" && shouldIncludePathDimension(input, "chat") {
		parts = append(parts, "chat_"+safePathID(value))
	}
	if spaceID := strings.TrimSpace(input.Context.SpaceID); spaceID != "" && !strings.EqualFold(spaceID, input.Context.ChatID) && shouldIncludePathDimension(input, "space") {
		spaceType := strings.TrimSpace(input.Context.SpaceType)
		if spaceType == "" {
			spaceType = "space"
		}
		parts = append(parts, "space_"+safePathID(spaceType+"_"+spaceID))
	} else if value := inputScopeValue(input, "space"); value != "" && shouldIncludePathDimension(input, "space") {
		parts = append(parts, "space_"+safePathID(value))
	}
	if senderID := strings.TrimSpace(input.Context.SenderID); senderID != "" && shouldIncludePathDimension(input, "sender") {
		parts = append(parts, "sender_"+safePathID(senderID))
	} else if value := inputScopeValue(input, "sender"); value != "" && shouldIncludePathDimension(input, "sender") {
		parts = append(parts, "sender_"+safePathID(value))
	}
	parts = append(parts, safePathID(input.SessionID))
	return strings.Join(parts, "__")
}

func shouldIncludePathDimension(input AgentTurnInput, dimension string) bool {
	if input.Scope == nil || len(input.Scope.Dimensions) == 0 {
		return true
	}
	dimension = strings.ToLower(strings.TrimSpace(dimension))
	for _, candidate := range input.Scope.Dimensions {
		if strings.EqualFold(strings.TrimSpace(candidate), dimension) {
			return true
		}
	}
	return false
}

func inputScopeValue(input AgentTurnInput, dimension string) string {
	if input.Scope == nil || len(input.Scope.Values) == 0 {
		return ""
	}
	return strings.TrimSpace(input.Scope.Values[strings.ToLower(strings.TrimSpace(dimension))])
}

func inputChannel(input AgentTurnInput) string {
	if channel := strings.TrimSpace(input.Context.Channel); channel != "" {
		return channel
	}
	if input.Scope != nil && strings.TrimSpace(input.Scope.Channel) != "" {
		return input.Scope.Channel
	}
	return "unknown-channel"
}

func inputEntrypointID(input AgentTurnInput) string {
	if entrypointID := strings.TrimSpace(input.Context.EntrypointID); entrypointID != "" {
		return entrypointID
	}
	if input.Scope != nil && strings.TrimSpace(input.Scope.EntrypointID) != "" {
		return input.Scope.EntrypointID
	}
	return "unknown-entrypoint"
}

func compactMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		msg.Role = strings.TrimSpace(msg.Role)
		msg.Kind = strings.TrimSpace(msg.Kind)
		if msg.Kind == "" {
			msg.Kind = MessageKindMessage
		}
		msg.Content = strings.TrimSpace(msg.Content)
		msg.ToolCallID = strings.TrimSpace(msg.ToolCallID)
		msg.ToolName = strings.TrimSpace(msg.ToolName)
		msg.AgentID = strings.TrimSpace(msg.AgentID)
		msg.RunID = strings.TrimSpace(msg.RunID)
		if msg.Role == "" || msg.Content == "" {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func latestMessageTime(messages []Message) time.Time {
	var latest time.Time
	for _, msg := range messages {
		if msg.CreatedAt.After(latest) {
			latest = msg.CreatedAt
		}
	}
	return latest
}

func appendJSONLines(path string, messages []Message) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open session messages: %w", err)
	}
	for _, msg := range messages {
		line, err := json.Marshal(msg)
		if err != nil {
			_ = file.Close()
			return fmt.Errorf("encode session message: %w", err)
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			_ = file.Close()
			return fmt.Errorf("append session message: %w", err)
		}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync session messages: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close session messages: %w", err)
	}
	return nil
}

func readMessages(path string) ([]Message, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open session messages: %w", err)
	}
	defer file.Close()

	var messages []Message
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		messages = append(messages, msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read session messages: %w", err)
	}
	return messages, nil
}

func readJSONFile[T any](path string) (T, error) {
	var value T
	content, err := os.ReadFile(path)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(content, &value); err != nil {
		return value, err
	}
	return value, nil
}

func writeJSONAtomic(path string, value any, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create session meta dir: %w", err)
	}
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session meta: %w", err)
	}
	content = append(content, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("create session meta temp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write session meta temp: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod session meta temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync session meta temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close session meta temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace session meta: %w", err)
	}
	cleanup = false
	return nil
}

func cloneScope(scope *SessionScope) *SessionScope {
	if scope == nil {
		return nil
	}
	out := *scope
	if len(scope.Dimensions) > 0 {
		out.Dimensions = append([]string(nil), scope.Dimensions...)
	}
	if len(scope.Values) > 0 {
		out.Values = map[string]string{}
		for key, value := range scope.Values {
			out.Values[key] = value
		}
	}
	return &out
}

// SafePathID 把任意字符串清洗成可安全用作路径段的形式：只保留字母/数字/-/_/.，
// 其余字符替换为 _，连续替换压缩为单个 _，首尾 _ 去除，空值变 "unknown"。
// 用于 per-sender 目录名（senderID 来自 IM，可能含 / 中文 空格等危险字符）。
// 导出供 tools 包 per-sender 数据隔离（#126）复用，避免两份清洗逻辑漂移。
func SafePathID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		keep := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.'
		if keep {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "unknown"
	}
	return out
}

// safePathID 是 SafePathID 的包内别名，保持现有内部调用不变。
var safePathID = SafePathID
