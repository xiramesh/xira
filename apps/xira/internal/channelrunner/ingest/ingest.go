// Package ingest 是 channel runner 和 runtime 之间的共享消息处理层。
//
// 职责（所有平台共有的，不该在三个 runner 里各写一遍）：
//   - 授权检查（allowlist + owner bypass + /bind pre-auth）
//   - 消息去重（dedupe）
//   - observe：群消息没 @ bot → 存 session，不触发 agent
//   - dispatch：@ bot → 存消息 + 触发 agent turn
//
// Runner 只管平台特有的事（解析消息格式、判断 mention），然后构造 MessageInput
// 交给 ingest。Ingest 统一决定 observe or dispatch。
//
// 架构决策（#151 comment）：SessionManager 是共享存储层（不归 Runtime 私有），
// Ingest 和 Runtime 都用它。未来压缩/提取/淘汰加在 SessionManager 或 Ingest 旁路，
// 不用改 Runner 或 Runtime。
package ingest

import (
	"context"
	"log/slog"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelrunner/dedupe"
	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/runtime"
	fsession "github.com/xiramesh/xira/internal/session"
)

// Decision 是 Ingest 对一条消息的处理决策。
type Decision int

const (
	// DecisionDispatch 表示消息已授权且该触发 agent turn。
	DecisionDispatch Decision = iota
	// DecisionObserve 表示群消息没 @ bot，已存进 session 但不触发 agent。
	DecisionObserve
	// DecisionReject 表示消息未授权或重复，已被拒绝。
	DecisionReject
)

// MessageInput 是 runner 解析完原始消息后构造的统一输入。
// Runner 只管填这个——后续的授权/dedupe/observe/dispatch 全由 Ingest 处理。
type MessageInput struct {
	Channel      string
	EntrypointID string
	Account      string
	ChatID       string
	ChatType     string // "group" / "direct" / "p2p"
	SenderID     string
	SenderName   string
	ChatName     string
	Mentioned    bool // feishu/ws 算好；ilink 无概念不填
	Content      string
	MessageID    string
	Metadata     map[string]string
}

// InboundContext 从 MessageInput 构造标准 InboundContext。
func (m MessageInput) InboundContext() channel.InboundContext {
	return channel.InboundContext{
		Channel:      m.Channel,
		EntrypointID: m.EntrypointID,
		Account:      m.Account,
		ChatID:       m.ChatID,
		ChatType:     m.ChatType,
		SenderID:     m.SenderID,
		SenderName:   m.SenderName,
		ChatName:     m.ChatName,
		Mentioned:    m.Mentioned,
		Raw:          m.Metadata,
	}
}

// Ingest 是统一的消息处理层。被注入到每个 channel runner。
type Ingest struct {
	sessionManager *fsession.Manager
	ownerResolver  runtime.OwnerResolver
}

// New 创建一个 Ingest。sessionManager 用于 observe（存消息）。
// ownerResolver 用于授权检查（owner bypass allowlist）。
func New(sessionManager *fsession.Manager, owner runtime.OwnerResolver) *Ingest {
	return &Ingest{
		sessionManager: sessionManager,
		ownerResolver:  owner,
	}
}

// AuthorizeSender 检查 sender 是否被授权使用该 entrypoint。
// 三处 runner 的 isAuthorizedSender 逻辑一字不差，抽到这统一维护。
//
// /bind pre-auth (#123)：/bind 指令绕过 allowlist（首次绑定时 sender 还不是 owner）。
// allowlist 命中 → 放行；owner bypass (#122) → 放行。
func AuthorizeSender(senderID, content string, def entrypoints.Definition, owner runtime.OwnerResolver) bool {
	if runtime.IsBindCommand(content) {
		return true
	}
	if def.AllowsSender(senderID) {
		return true
	}
	if owner == nil {
		return false
	}
	return owner.IsOwner(context.Background(), senderID, def.ID)
}

// Gate 评估一条消息的处理决策（observe / dispatch / reject）。
//
// 逻辑：
//   - 私聊（非 group）：授权 → dispatch；未授权 → reject
//   - 群聊 @ bot：授权 → dispatch；未授权 → reject
//   - 群聊没 @ bot：授权 → observe；未授权 → reject（不 observe 未授权内容，安全）
func (ing *Ingest) Gate(input MessageInput, def entrypoints.Definition) Decision {
	authorized := AuthorizeSender(input.SenderID, input.Content, def, ing.ownerResolver)
	if !authorized {
		return DecisionReject
	}
	if input.ChatType == "group" && !input.Mentioned {
		return DecisionObserve
	}
	return DecisionDispatch
}

// Observe 将一条群消息存进 session 历史，不触发 agent turn。
// 只对已授权的消息调用（安全：未授权内容不进共享 session）。
//
// dedupeKey 用于防止飞书/ilink 重投同一消息导致重复 observe。
// 空 dedupeKey 表示不做 dedupe。
func (ing *Ingest) Observe(input MessageInput, def entrypoints.Definition, dedupeKey string, deduper *dedupe.MessageDeduper) {
	if ing.sessionManager == nil {
		return
	}
	// dedupe：防重投
	if deduper != nil && dedupeKey != "" {
		if !deduper.Begin(dedupeKey, time.Now()) {
			slog.Debug("ingest: observe duplicate skipped", "message_id", input.MessageID)
			return
		}
		// observe 后立即 complete（observe 不是 turn，不需要长时间持锁）
		defer deduper.Complete(dedupeKey, time.Now())
	}

	inbound := input.InboundContext()
	allocation := ing.sessionManager.Allocate(fsession.AllocationInput{
		Context:       inbound,
		SessionPolicy: def.SessionPolicy,
	})
	if err := ing.sessionManager.AppendAgentMessages(fsession.AgentTurnInput{
		SessionID: allocation.SessionID,
		AgentID:   def.DefaultAgentID,
		Context:   inbound,
		Scope:     &allocation.Scope,
	}, []fsession.Message{{
		Role:       "user",
		Kind:       "message",
		Content:    input.Content,
		SenderID:   input.SenderID,
		SenderName: input.SenderName,
		CreatedAt:  time.Now().UTC(),
	}}); err != nil {
		slog.Warn("ingest: observe failed",
			"entrypoint_id", def.ID, "chat_id", input.ChatID, "err", err)
	}
	slog.Info("ingest: message observed",
		"entrypoint_id", def.ID,
		"chat_id", input.ChatID,
		"sender_id", input.SenderID,
		"message_id", input.MessageID,
	)
}
