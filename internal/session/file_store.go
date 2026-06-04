package session

import (
	"bufio"
	"encoding/json"
	"fmt"
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
	if s == nil {
		return nil
	}
	messages = compactMessages(messages)
	if len(messages) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	conversationDir := s.conversationDir(input.SessionID)
	agentDir := s.agentDir(input.SessionID, input.AgentID)
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
	entries, err := os.ReadDir(s.root)
	if os.IsNotExist(err) {
		return histories, agentHistories, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read session store: %w", err)
	}
	type orderedMessage struct {
		message Message
		order   int
	}
	order := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		conversationDir := filepath.Join(s.root, entry.Name())
		meta, err := readJSONFile[ConversationMeta](filepath.Join(conversationDir, "meta.json"))
		if err != nil || strings.TrimSpace(meta.SessionID) == "" {
			continue
		}
		agentsDir := filepath.Join(conversationDir, "agents")
		agentEntries, err := os.ReadDir(agentsDir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read session agents: %w", err)
		}
		var combined []orderedMessage
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
				return nil, nil, err
			}
			for i := range messages {
				if messages[i].AgentID == "" {
					messages[i].AgentID = agentID
				}
				combined = append(combined, orderedMessage{message: messages[i], order: order})
				order++
			}
			if len(messages) > 0 {
				if agentHistories[meta.SessionID] == nil {
					agentHistories[meta.SessionID] = map[string][]Message{}
				}
				agentHistories[meta.SessionID][agentID] = append([]Message(nil), messages...)
			}
		}
		sort.SliceStable(combined, func(i, j int) bool {
			left := combined[i].message.CreatedAt
			right := combined[j].message.CreatedAt
			if left.IsZero() || right.IsZero() || left.Equal(right) {
				return combined[i].order < combined[j].order
			}
			return left.Before(right)
		})
		for _, item := range combined {
			histories[meta.SessionID] = append(histories[meta.SessionID], item.message)
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

func compactMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		msg.Role = strings.TrimSpace(msg.Role)
		msg.Content = strings.TrimSpace(msg.Content)
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

func safePathID(value string) string {
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
