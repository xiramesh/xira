package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"

	"github.com/xiramesh/xira/internal/channel"
)

const (
	notifyOwnerToolName        = "notify_owner"
	notifyOwnerMaxMessageRunes = 4000
)

type OwnerDeliveryTarget struct {
	Route     channel.InboundContext
	Recipient channel.OutboundRecipient
}

type notifyOwnerRunState struct {
	mu   sync.Mutex
	sent bool
}

type notifyOwnerRunStateKey struct{}

func contextWithNotifyOwnerRunState(ctx context.Context) context.Context {
	return context.WithValue(ctx, notifyOwnerRunStateKey{}, &notifyOwnerRunState{})
}

func notifyOwnerRunStateFromContext(ctx context.Context) *notifyOwnerRunState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(notifyOwnerRunStateKey{}).(*notifyOwnerRunState)
	return state
}

// ResolveOwnerTarget resolves a private delivery target without changing the
// authorization-only IsOwner contract. Dynamic binding wins over static
// configuration, matching IsOwner. Missing identity type fails closed.
// coverage: contract (100% required)
func (s *Service) ResolveOwnerTarget(_ context.Context, entrypointID string) (OwnerDeliveryTarget, error) {
	if s == nil || s.entrypoints == nil {
		return OwnerDeliveryTarget{}, errors.New("owner target resolver is not configured")
	}
	entrypointID = strings.TrimSpace(entrypointID)
	if entrypointID == "" {
		return OwnerDeliveryTarget{}, errors.New("entrypoint id is required")
	}
	definition, ok := s.entrypoints.Definition(entrypointID)
	if !ok {
		return OwnerDeliveryTarget{}, fmt.Errorf("entrypoint %q not found", entrypointID)
	}

	ownerID := ""
	ownerIDType := ""
	if s.ownerBindings != nil {
		if binding, found := s.ownerBindings.Get(entrypointID); found {
			ownerID = strings.TrimSpace(binding.OwnerSenderID)
			ownerIDType = strings.ToLower(strings.TrimSpace(binding.OwnerSenderIDType))
		}
	}
	if ownerID == "" {
		ownerID = strings.TrimSpace(definition.OwnerID)
		ownerIDType = strings.ToLower(strings.TrimSpace(definition.OwnerIDType))
	}
	if ownerID == "" {
		return OwnerDeliveryTarget{}, fmt.Errorf("entrypoint %q has no owner", entrypointID)
	}
	if ownerIDType == "" {
		return OwnerDeliveryTarget{}, fmt.Errorf("owner binding for entrypoint %q has no id type; owner must rebind or configure owner_id_type", entrypointID)
	}

	return OwnerDeliveryTarget{
		Route: channel.InboundContext{
			Channel:      definition.Channel,
			EntrypointID: definition.ID,
			Account:      definition.Account,
			ChannelAppID: definition.AppID,
			BotID:        definition.BotID,
		},
		Recipient: channel.OutboundRecipient{ID: ownerID, IDType: ownerIDType}.Normalize(),
	}, nil
}

func (s *Service) notifyOwnerToolCall(ctx context.Context, callID string, args map[string]any) ToolCallRecord {
	started := time.Now()
	if strings.TrimSpace(callID) == "" {
		callID = uuid.NewString()
	}
	message, _ := args["message"].(string)
	message = strings.TrimSpace(message)
	rec := ToolCallRecord{
		ID:        callID,
		Name:      notifyOwnerToolName,
		Input:     map[string]any{"message_chars": utf8.RuneCountInString(message)},
		StartedAt: started,
	}
	reject := func(err error) ToolCallRecord {
		rec.Error = err.Error()
		rec.Output = map[string]any{"status": "rejected", "error": err.Error()}
		rec.EndedAt = time.Now()
		return rec
	}
	if message == "" {
		return reject(errors.New("message is required"))
	}
	if utf8.RuneCountInString(message) > notifyOwnerMaxMessageRunes {
		return reject(fmt.Errorf("message exceeds %d characters", notifyOwnerMaxMessageRunes))
	}
	exec, ok := runExecutionFromContext(ctx)
	if !ok {
		return reject(errors.New("notify_owner requires runtime execution context"))
	}
	state := notifyOwnerRunStateFromContext(ctx)
	if state != nil {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.sent {
			return reject(errors.New("owner notification already sent for this run"))
		}
	}
	entrypointID := strings.TrimSpace(exec.Base.EntrypointID)
	if entrypointID == "" {
		entrypointID = strings.TrimSpace(exec.Request.Context.EntrypointID)
	}
	target, err := s.ResolveOwnerTarget(ctx, entrypointID)
	if err != nil {
		return reject(err)
	}
	if s.outbound == nil {
		rec.Error = "outbound emitter is not configured"
		rec.Output = map[string]any{"status": "failed", "error": rec.Error}
		rec.EndedAt = time.Now()
		return rec
	}
	if !s.outbound.Capabilities().Supports(channel.CapabilityTypedRecipientOutbound) {
		rec.Error = "outbound emitter does not support typed recipient outbound"
		rec.Output = map[string]any{"status": "failed", "error": rec.Error}
		rec.EndedAt = time.Now()
		return rec
	}

	env := channel.NewOutboundEnvelope(channel.OutboundProactiveMessage)
	env.ID = callID
	env.RunID = exec.Base.RunID
	source := exec.Request.Context
	route := target.Route
	recipient := target.Recipient
	env.Source = &source
	env.Target = &route
	env.Recipient = &recipient
	env.Correlation = channel.OutboundCorrelation{TraceID: exec.Base.TraceID, ToolCallID: callID}
	env.Data = map[string]any{"content": message}
	if err := s.outbound.Emit(ctx, env); err != nil {
		rec.Error = err.Error()
		rec.Output = map[string]any{"status": "failed", "error": err.Error(), "entrypoint_id": entrypointID}
		rec.EndedAt = time.Now()
		return rec
	}
	rec.Output = map[string]any{"status": "sent", "entrypoint_id": entrypointID}
	rec.EndedAt = time.Now()
	if state != nil {
		state.sent = true
	}
	return rec
}

func notifyOwnerInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"message": {Type: "string", MaxLength: intPtr(notifyOwnerMaxMessageRunes)},
		},
		Required:             []string{"message"},
		AdditionalProperties: rejectAllSchema(),
	}
}

func intPtr(value int) *int { return &value }

func recordNotifyOwnerOutcome(
	rec ToolCallRecord,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) {
	status, _ := rec.Output["status"].(string)
	entrypointID, _ := rec.Output["entrypoint_id"].(string)
	payload := map[string]any{
		"status":        status,
		"tool_call_id":  rec.ID,
		"entrypoint_id": entrypointID,
		"message_chars": rec.Input["message_chars"],
	}
	if rec.Error != "" {
		payload["error"] = rec.Error
	}
	recordEvent("owner.notification", "runtime", "owner notification "+status, payload)
	recordAudit(notifyOwnerToolName, entrypointID, status == "sent", "owner notification "+status, map[string]any{
		"tool_call_id":  rec.ID,
		"message_chars": rec.Input["message_chars"],
		"status":        status,
	})
}

// hasSuccessfulNotifyOwner is the narrow intentional-silence contract: an
// empty final is valid only after the private side effect actually succeeded.
// coverage: contract (100% required)
func hasSuccessfulNotifyOwner(records []ToolCallRecord) bool {
	notified := false
	for _, record := range records {
		if record.Error != "" {
			return false
		}
		status, _ := record.Output["status"].(string)
		if status == "failed" || status == "rejected" {
			return false
		}
		if record.Name == notifyOwnerToolName && status == "sent" {
			notified = true
		}
	}
	return notified
}
