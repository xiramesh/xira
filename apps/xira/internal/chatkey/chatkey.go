// Package chatkey 定义 per-turn 的路由身份 ChatKey 及其在 context 里的传播。
//
// 本包独立于 runtime，供 tools / runtime / channelrunner 共用，避免循环依赖
// （tools 需要从 ctx 取 SenderID 做 per-sender 数据隔离 #126，但不能 import runtime）。
//
// 语义背景见 runtime 的 RFC xira-per-chat-key-architecture-rfc-v0.zh.md §2.1。
package chatkey

import "context"

// ChatKey 唯一标识一个 sender 视角下的一次会话。
// ChatKey = (Channel, ChatID, SenderID)。相同 chat key = 相同 turn scope。
//
// DataIsolation 标记该 turn 的 entrypoint 是否启用了 per-sender 数据隔离（#126）。
// 工具（write_file 等）读它决定是否走 overlay 解析——只有 DataIsolation=true 且
// SenderID 非空时才隔离，否则走单层（向后兼容 CLI/TUI 等不配隔离的场景）。
type ChatKey struct {
	Channel       string
	ChatID        string
	SenderID      string
	DataIsolation bool
}

// String 返回稳定的、人类可读的表示，用于日志/调试。
// 格式 "channel/chat/sender"，ParseChatKey（runtime 包）是其逆运算。
func (k ChatKey) String() string {
	return k.Channel + "/" + k.ChatID + "/" + k.SenderID
}

// chatKeyContextKey 携带当前 turn 的 ChatKey。由 runtime 在 RunAgent 入口注入
// （WithChatKey），spawned children 读取它注册到 per-chat-key cancel registry。
type chatKeyContextKey struct{}

// WithChatKey 返回携带 chatKey 的 ctx。
func WithChatKey(ctx context.Context, key ChatKey) context.Context {
	return context.WithValue(ctx, chatKeyContextKey{}, key)
}

// FromContext 返回 ctx 里的 ChatKey（如果存在）。
func FromContext(ctx context.Context) (ChatKey, bool) {
	k, ok := ctx.Value(chatKeyContextKey{}).(ChatKey)
	return k, ok
}

// SenderIDFromContext 是 FromContext 的便捷封装：只取 SenderID。
// tools 包用这个做 per-sender 数据隔离（#126）。
func SenderIDFromContext(ctx context.Context) (string, bool) {
	k, ok := FromContext(ctx)
	if !ok {
		return "", false
	}
	return k.SenderID, true
}

// DataIsolationEnabledFromContext 报告当前 turn 的 entrypoint 是否启用了
// per-sender 数据隔离（#126）。工具读它 + SenderID 决定是否走 overlay。
func DataIsolationEnabledFromContext(ctx context.Context) bool {
	k, ok := FromContext(ctx)
	if !ok {
		return false
	}
	return k.DataIsolation
}
