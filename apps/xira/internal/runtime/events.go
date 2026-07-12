package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	fsession "github.com/xiramesh/xira/internal/session"
)

type runtimeEventBase struct {
	RunID                 string
	AgentID               string
	ChildAgentID          string
	EntrypointID          string
	Channel               string
	Account               string
	ChannelAppID          string
	BotID                 string
	ConversationSessionID string
	AgentSessionID        string
	ChatID                string
	ChatType              string
	ChatName              string
	TopicID               string
	SpaceID               string
	SpaceType             string
	SenderID              string
	SenderName            string
	MessageID             string
	ReplyToMessageID      string
	ReplyToSenderID       string
	DelegationDepth       int
	TraceID               string
}

type runtimeEventCorrelationInput struct {
	ParentRunID string
	ChildRunID  string
	ToolCallID  string
}

type runExecutionContext struct {
	Base        runtimeEventBase
	Profile     agents.Profile
	Request     TurnRequest
	UserMessage string
}

type runExecutionContextKey struct{}

func contextWithRunExecution(ctx context.Context, exec runExecutionContext) context.Context {
	return context.WithValue(ctx, runExecutionContextKey{}, exec)
}

func runExecutionFromContext(ctx context.Context) (runExecutionContext, bool) {
	exec, ok := ctx.Value(runExecutionContextKey{}).(runExecutionContext)
	return exec, ok
}

// inboundContextFromScope reconstructs a channel.InboundContext from a
// persisted session scope. Resume paths (HITL/delegation) no longer have the
// original flattened Channel/UserID/Metadata, but the durable session scope
// carries the trigger identity (channel/chat/sender/space). This rebuilds the
// first-class context so resumed runs land under the same conversation tree
// instead of a forged "resume" channel.
func inboundContextFromScope(scope *fsession.SessionScope, raw map[string]string) channel.InboundContext {
	if scope == nil {
		return channel.NormalizeInboundContext(channel.InboundContext{Raw: raw})
	}
	ctx := channel.InboundContext{
		Channel:      scope.Channel,
		EntrypointID: scope.EntrypointID,
		Account:      scope.Account,
		Raw:          raw,
	}
	// SessionScope.Values encode dimensions as "<type>:<id>" (chat/space/topic)
	// or canonical sender ids. Recover the raw ids.
	ctx.ChatID = scopeValueID(scope.Values["chat"])
	ctx.ChatType = scopeValueType(scope.Values["chat"])
	ctx.SpaceID = scopeValueID(scope.Values["space"])
	ctx.SpaceType = scopeValueType(scope.Values["space"])
	ctx.TopicID = scopeValueID(scope.Values["topic"])
	// #151：sender 不再是 dimension（不在 scope.Values 里）。
	// sender 的 canonical id 存在 scope.Names["sender_id"]（不参与 session ID 计算，
	// 但 resume delivery 需要它路由）。Strip canonical prefix 同 #71。
	// 旧格式 fallback：#151 之前的 run 用 Values["sender"]，需要兼容。
	if scope.Names != nil && scope.Names["sender_id"] != "" {
		ctx.SenderID = scopeValueID(scope.Names["sender_id"])
	} else if scope.Values["sender"] != "" {
		ctx.SenderID = scopeValueID(scope.Values["sender"])
	}
	// Names travel alongside Values (scope.Names) but are not encoded as
	// "<type>:<id>" — they're plain display strings. Restore as-is.
	if scope.Names != nil {
		ctx.ChatName = scope.Names["chat_name"]
		ctx.SenderName = scope.Names["sender_name"]
	}
	return channel.NormalizeInboundContext(ctx)
}

// scopeValueID extracts the id portion of a "<type>:<id>" scope value.
func scopeValueID(scopeValue string) string {
	if idx := strings.Index(scopeValue, ":"); idx >= 0 {
		return scopeValue[idx+1:]
	}
	return scopeValue
}

// scopeValueType extracts the type portion of a "<type>:<id>" scope value.
func scopeValueType(scopeValue string) string {
	if idx := strings.Index(scopeValue, ":"); idx >= 0 {
		return scopeValue[:idx]
	}
	return ""
}

func newRuntimeEvent(base runtimeEventBase, kind, source, message string, payload map[string]any, correlation *runtimeEventCorrelationInput) RuntimeEvent {
	eventPayload := copyAnyMap(payload)
	if eventPayload == nil {
		eventPayload = map[string]any{}
	}
	if correlation == nil {
		correlation = correlationFromPayload(eventPayload)
	}
	if childAgentID := anyString(eventPayload["child_agent_id"]); childAgentID != "" && base.ChildAgentID == "" {
		base.ChildAgentID = childAgentID
	}
	addPayloadString(eventPayload, "run_id", base.RunID)
	addPayloadString(eventPayload, "agent_id", base.AgentID)
	addPayloadString(eventPayload, "child_agent_id", base.ChildAgentID)
	addPayloadString(eventPayload, "entrypoint_id", base.EntrypointID)
	addPayloadString(eventPayload, "channel", base.Channel)
	addPayloadString(eventPayload, "account", base.Account)
	addPayloadString(eventPayload, "channel_app_id", base.ChannelAppID)
	addPayloadString(eventPayload, "bot_id", base.BotID)
	addPayloadString(eventPayload, "conversation_session_id", base.ConversationSessionID)
	addPayloadString(eventPayload, "agent_session_id", base.AgentSessionID)
	addPayloadString(eventPayload, "chat_id", base.ChatID)
	addPayloadString(eventPayload, "chat_type", base.ChatType)
	if correlation != nil {
		addPayloadString(eventPayload, "parent_run_id", correlation.ParentRunID)
		addPayloadString(eventPayload, "child_run_id", correlation.ChildRunID)
		addPayloadString(eventPayload, "tool_call_id", correlation.ToolCallID)
	}
	return RuntimeEvent{
		ID:            uuid.NewString(),
		SchemaVersion: 1,
		RunID:         base.RunID,
		Kind:          strings.TrimSpace(kind),
		Time:          time.Now(),
		Source:        strings.TrimSpace(source),
		SourceDetail:  sourceDetail(source),
		Scope:         eventScope(base),
		Correlation:   eventCorrelation(base, correlation),
		Severity:      "info",
		Message:       strings.TrimSpace(message),
		Payload:       eventPayload,
	}
}

func copyAnyMap(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func addPayloadString(payload map[string]any, key, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	payload[key] = value
}

func sourceDetail(source string) *RuntimeEventSource {
	source = strings.TrimSpace(source)
	switch {
	case source == "":
		return &RuntimeEventSource{Component: "runtime"}
	case source == "runtime":
		return &RuntimeEventSource{Component: "runtime", Name: "runtime"}
	case strings.HasPrefix(source, "adk."):
		return &RuntimeEventSource{Component: "adk", Name: strings.TrimPrefix(source, "adk.")}
	case strings.Contains(source, "."):
		return &RuntimeEventSource{Component: "tool", Name: source}
	default:
		return &RuntimeEventSource{Component: source, Name: source}
	}
}

func eventScope(base runtimeEventBase) *RuntimeEventScope {
	return &RuntimeEventScope{
		EntrypointID:          base.EntrypointID,
		Channel:               base.Channel,
		Account:               base.Account,
		ChannelAppID:          base.ChannelAppID,
		BotID:                 base.BotID,
		ConversationSessionID: base.ConversationSessionID,
		AgentSessionID:        base.AgentSessionID,
		RunID:                 base.RunID,
		AgentID:               base.AgentID,
		ChildAgentID:          base.ChildAgentID,
		ChatID:                base.ChatID,
		ChatType:              base.ChatType,
		TopicID:               base.TopicID,
		SpaceID:               base.SpaceID,
		SpaceType:             base.SpaceType,
		SenderID:              base.SenderID,
		MessageID:             base.MessageID,
		ReplyToMessageID:      base.ReplyToMessageID,
		ReplyToSenderID:       base.ReplyToSenderID,
		DelegationDepth:       base.DelegationDepth,
	}
}

func eventCorrelation(base runtimeEventBase, input *runtimeEventCorrelationInput) *RuntimeEventCorrelation {
	if strings.TrimSpace(base.TraceID) == "" && input == nil {
		return nil
	}
	out := &RuntimeEventCorrelation{TraceID: base.TraceID}
	if input != nil {
		out.ParentRunID = input.ParentRunID
		out.ChildRunID = input.ChildRunID
		out.ToolCallID = input.ToolCallID
	}
	return out
}

func correlationFromPayload(payload map[string]any) *runtimeEventCorrelationInput {
	parentRunID := anyString(payload["parent_run_id"])
	childRunID := anyString(payload["child_run_id"])
	toolCallID := anyString(payload["tool_call_id"])
	if parentRunID == "" && childRunID == "" && toolCallID == "" {
		return nil
	}
	return &runtimeEventCorrelationInput{
		ParentRunID: parentRunID,
		ChildRunID:  childRunID,
		ToolCallID:  toolCallID,
	}
}

func anyString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}

// waitingHumanSummary derives a human-facing summary for the run.waiting_human
// event payload. The progress forwarder renders it directly into IM chat as an
// interaction signal ("need your confirmation: ..."). It prefers the first
// pending human-request question (already phrased for the user) and falls back
// to the interrupt reason. This is the only payload field the renderer may
// splice into chat text verbatim — see progress feed §14.
func waitingHumanSummary(interrupt *RunInterrupt) string {
	if interrupt == nil {
		return ""
	}
	for _, hr := range interrupt.HumanRequests {
		if q := strings.TrimSpace(hr.Question); q != "" {
			return q
		}
	}
	return strings.TrimSpace(interrupt.Reason)
}
