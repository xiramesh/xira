package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ai-daming/xira/internal/channel"
	"github.com/ai-daming/xira/internal/routing"
)

const ScopeVersionV1 = 1

type SessionScope struct {
	Version      int               `json:"version" yaml:"version"`
	EntrypointID string            `json:"entrypoint_id" yaml:"entrypoint_id"`
	Channel      string            `json:"channel" yaml:"channel"`
	Account      string            `json:"account,omitempty" yaml:"account,omitempty"`
	Dimensions   []string          `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	Values       map[string]string `json:"values,omitempty" yaml:"values,omitempty"`
}

type AllocationInput struct {
	Context           channel.InboundContext
	SessionPolicy     routing.SessionPolicy
	SessionIDOverride string
}

type Allocation struct {
	Scope     SessionScope
	SessionID string
}

type Message struct {
	Role      string    `json:"role" yaml:"role"`
	Content   string    `json:"content" yaml:"content"`
	CreatedAt time.Time `json:"created_at,omitempty" yaml:"created_at,omitempty"`
	AgentID   string    `json:"agent_id,omitempty" yaml:"agent_id,omitempty"`
	RunID     string    `json:"run_id,omitempty" yaml:"run_id,omitempty"`
}

type AgentTurnInput struct {
	SessionID      string
	AgentID        string
	AgentSessionID string
	RunID          string
	Context        channel.InboundContext
	Scope          *SessionScope
	UserMessage    string
	AssistantReply string
}

type Manager struct {
	mu             sync.RWMutex
	histories      map[string][]Message
	agentHistories map[string]map[string][]Message
	store          *FileStore
}

func NewManager() *Manager {
	return &Manager{
		histories:      map[string][]Message{},
		agentHistories: map[string]map[string][]Message{},
	}
}

func NewManagerWithStore(root string) (*Manager, error) {
	manager := NewManager()
	root = strings.TrimSpace(root)
	if root == "" {
		return manager, nil
	}
	store, err := NewFileStore(root)
	if err != nil {
		return nil, err
	}
	histories, agentHistories, err := store.LoadHistories()
	if err != nil {
		return nil, err
	}
	manager.histories = histories
	manager.agentHistories = agentHistories
	manager.store = store
	return manager, nil
}

func (m *Manager) Root() string {
	if m == nil || m.store == nil {
		return ""
	}
	return m.store.Root()
}

func (m *Manager) Allocate(input AllocationInput) Allocation {
	scope := BuildScope(input.Context, input.SessionPolicy)
	sessionID := strings.TrimSpace(input.SessionIDOverride)
	if sessionID == "" {
		sessionID = BuildSessionID(scope)
	}
	return Allocation{Scope: scope, SessionID: sessionID}
}

func BuildScope(ctx channel.InboundContext, policy routing.SessionPolicy) SessionScope {
	ctx = channel.NormalizeInboundContext(ctx)
	scope := SessionScope{
		Version:      ScopeVersionV1,
		EntrypointID: strings.TrimSpace(ctx.EntrypointID),
		Channel:      ctx.Channel,
		Account:      strings.TrimSpace(ctx.Account),
	}
	values := map[string]string{}
	for _, dimension := range policy.Dimensions {
		switch strings.ToLower(strings.TrimSpace(dimension)) {
		case "space":
			if ctx.SpaceID == "" {
				continue
			}
			spaceType := strings.TrimSpace(ctx.SpaceType)
			if spaceType == "" {
				spaceType = "space"
			}
			values["space"] = strings.ToLower(spaceType) + ":" + strings.ToLower(ctx.SpaceID)
		case "chat":
			if ctx.ChatID == "" {
				continue
			}
			chatType := strings.TrimSpace(ctx.ChatType)
			if chatType == "" {
				chatType = "direct"
			}
			values["chat"] = strings.ToLower(chatType) + ":" + strings.ToLower(ctx.ChatID)
		case "topic":
			if ctx.TopicID == "" {
				continue
			}
			values["topic"] = "topic:" + strings.ToLower(ctx.TopicID)
		case "sender":
			if ctx.SenderID == "" {
				continue
			}
			values["sender"] = canonicalSenderID(ctx.Channel, ctx.SenderID, policy.IdentityLinks)
		case "channel":
			if ctx.Channel == "" {
				continue
			}
			values["channel"] = "channel:" + strings.ToLower(ctx.Channel)
		}
	}
	if len(values) > 0 {
		scope.Dimensions = sortedKeys(values)
		scope.Values = values
	}
	return scope
}

func BuildSessionID(scope SessionScope) string {
	signature := scopeSignature(scope)
	sum := sha256.Sum256([]byte(signature))
	return fmt.Sprintf("conversation:%s", hex.EncodeToString(sum[:])[:16])
}

func BuildAgentSessionID(conversationID, agentID string) string {
	conversationID = strings.ToLower(strings.TrimSpace(conversationID))
	if conversationID == "" {
		conversationID = "conversation:unknown"
	}
	signature := "conversation=" + conversationID + "|agent=" + strings.ToLower(strings.TrimSpace(agentID))
	sum := sha256.Sum256([]byte(signature))
	return fmt.Sprintf("session:%s:%s", safeID(agentID), hex.EncodeToString(sum[:])[:16])
}

func (m *Manager) AppendMessage(sessionID string, msg Message) {
	sessionID = strings.TrimSpace(sessionID)
	msg.Role = strings.TrimSpace(msg.Role)
	msg.Content = strings.TrimSpace(msg.Content)
	if m == nil || sessionID == "" || msg.Role == "" || msg.Content == "" {
		return
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.histories[sessionID] = append(m.histories[sessionID], msg)
}

func (m *Manager) AppendTurn(sessionID, userMessage, assistantMessage string) {
	m.AppendMessage(sessionID, Message{Role: "user", Content: userMessage})
	m.AppendMessage(sessionID, Message{Role: "assistant", Content: assistantMessage})
}

func (m *Manager) AppendAgentTurn(input AgentTurnInput) error {
	if m == nil {
		return nil
	}
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.AgentID = strings.TrimSpace(input.AgentID)
	input.AgentSessionID = strings.TrimSpace(input.AgentSessionID)
	if input.SessionID == "" || input.AgentID == "" {
		return nil
	}
	now := time.Now()
	messages := []Message{
		{
			Role:      "user",
			Content:   strings.TrimSpace(input.UserMessage),
			CreatedAt: now,
			AgentID:   input.AgentID,
			RunID:     strings.TrimSpace(input.RunID),
		},
		{
			Role:      "assistant",
			Content:   strings.TrimSpace(input.AssistantReply),
			CreatedAt: now.Add(time.Nanosecond),
			AgentID:   input.AgentID,
			RunID:     strings.TrimSpace(input.RunID),
		},
	}
	messages = compactMessages(messages)
	if len(messages) == 0 {
		return nil
	}

	m.mu.Lock()
	m.histories[input.SessionID] = append(m.histories[input.SessionID], messages...)
	if m.agentHistories[input.SessionID] == nil {
		m.agentHistories[input.SessionID] = map[string][]Message{}
	}
	m.agentHistories[input.SessionID][input.AgentID] = append(m.agentHistories[input.SessionID][input.AgentID], messages...)
	store := m.store
	m.mu.Unlock()

	if store == nil {
		return nil
	}
	return store.AppendAgentTurn(input, messages)
}

func (m *Manager) History(sessionID string) []Message {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	history := m.histories[strings.TrimSpace(sessionID)]
	return append([]Message(nil), history...)
}

func (m *Manager) AgentHistory(sessionID, agentID string) []Message {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	byAgent := m.agentHistories[strings.TrimSpace(sessionID)]
	history := byAgent[strings.TrimSpace(agentID)]
	return append([]Message(nil), history...)
}

func (m *Manager) LastMessages(sessionID string, max int) []Message {
	history := m.History(sessionID)
	if max > 0 && len(history) > max {
		history = history[len(history)-max:]
	}
	return history
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func scopeSignature(scope SessionScope) string {
	var b strings.Builder
	b.WriteString("v=")
	b.WriteString(fmt.Sprint(scope.Version))
	b.WriteString("|entrypoint=")
	b.WriteString(strings.ToLower(strings.TrimSpace(scope.EntrypointID)))
	b.WriteString("|channel=")
	b.WriteString(strings.ToLower(strings.TrimSpace(scope.Channel)))
	b.WriteString("|account=")
	b.WriteString(strings.ToLower(strings.TrimSpace(scope.Account)))
	for _, dimension := range scope.Dimensions {
		key := strings.ToLower(strings.TrimSpace(dimension))
		b.WriteString("|")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(strings.ToLower(strings.TrimSpace(scope.Values[key])))
	}
	return b.String()
}

func safeID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("/", "-", " ", "-", ":", "-").Replace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func canonicalSenderID(channelName, senderID string, identityLinks map[string][]string) string {
	senderID = strings.ToLower(strings.TrimSpace(senderID))
	channelName = strings.ToLower(strings.TrimSpace(channelName))
	if senderID == "" {
		return ""
	}
	candidate := channelName + ":" + senderID
	for canonical, aliases := range identityLinks {
		for _, alias := range aliases {
			if strings.EqualFold(strings.TrimSpace(alias), candidate) || strings.EqualFold(strings.TrimSpace(alias), senderID) {
				return strings.ToLower(strings.TrimSpace(canonical))
			}
		}
	}
	return candidate
}
