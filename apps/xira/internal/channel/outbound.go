package channel

import (
	"context"
	"sort"
	"strings"
	"time"
)

const ContractSchemaVersion = "xira.channel.v0"

type OutboundType string

const (
	OutboundAck              OutboundType = "ack"
	OutboundRuntimeEvent     OutboundType = "runtime_event"
	OutboundAssistantDelta   OutboundType = "assistant_delta"
	OutboundAssistantFinal   OutboundType = "assistant_final"
	OutboundInterrupt        OutboundType = "interrupt"
	OutboundProactiveMessage OutboundType = "outbound_message"
	OutboundError            OutboundType = "error"
)

func (outboundType OutboundType) IsValid() bool {
	switch outboundType {
	case OutboundAck,
		OutboundRuntimeEvent,
		OutboundAssistantDelta,
		OutboundAssistantFinal,
		OutboundInterrupt,
		OutboundProactiveMessage,
		OutboundError:
		return true
	default:
		return false
	}
}

type Capability string

const (
	CapabilityStreamingDelta           Capability = "streaming_delta"
	CapabilityRuntimeEventStream       Capability = "runtime_event_stream"
	CapabilityInteractiveHumanResponse Capability = "interactive_human_response"
	CapabilityProactiveOutbound        Capability = "proactive_outbound"
	CapabilityTypedRecipientOutbound   Capability = "typed_recipient_outbound"
	CapabilityOfflineQueue             Capability = "offline_queue"
)

type CapabilitySet []Capability

func (set CapabilitySet) Supports(capability Capability) bool {
	for _, candidate := range set {
		if candidate == capability {
			return true
		}
	}
	return false
}

func NormalizeCapabilities(capabilities []Capability) CapabilitySet {
	seen := map[Capability]struct{}{}
	out := make(CapabilitySet, 0, len(capabilities))
	for _, capability := range capabilities {
		capability = Capability(strings.TrimSpace(string(capability)))
		if capability == "" {
			continue
		}
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		out = append(out, capability)
	}
	return out
}

type OutboundCorrelation struct {
	TraceID         string `json:"trace_id,omitempty" yaml:"trace_id,omitempty"`
	ParentRunID     string `json:"parent_run_id,omitempty" yaml:"parent_run_id,omitempty"`
	ChildRunID      string `json:"child_run_id,omitempty" yaml:"child_run_id,omitempty"`
	ParentEventID   string `json:"parent_event_id,omitempty" yaml:"parent_event_id,omitempty"`
	ToolCallID      string `json:"tool_call_id,omitempty" yaml:"tool_call_id,omitempty"`
	ParentMessageID string `json:"parent_message_id,omitempty" yaml:"parent_message_id,omitempty"`
}

type OutboundRecipient struct {
	ID     string `json:"id" yaml:"id"`
	IDType string `json:"id_type" yaml:"id_type"`
}

// Normalize keeps the recipient contract deterministic. Channel adapters own
// the sealed set of ID types they accept; the shared contract only requires a
// non-empty typed identity.
// coverage: contract (100% required)
func (recipient OutboundRecipient) Normalize() OutboundRecipient {
	recipient.ID = strings.TrimSpace(recipient.ID)
	recipient.IDType = strings.ToLower(strings.TrimSpace(recipient.IDType))
	return recipient
}

func (correlation OutboundCorrelation) Normalize() OutboundCorrelation {
	correlation.TraceID = strings.TrimSpace(correlation.TraceID)
	correlation.ParentRunID = strings.TrimSpace(correlation.ParentRunID)
	correlation.ChildRunID = strings.TrimSpace(correlation.ChildRunID)
	correlation.ParentEventID = strings.TrimSpace(correlation.ParentEventID)
	correlation.ToolCallID = strings.TrimSpace(correlation.ToolCallID)
	correlation.ParentMessageID = strings.TrimSpace(correlation.ParentMessageID)
	return correlation
}

type OutboundEnvelope struct {
	SchemaVersion string              `json:"schema_version" yaml:"schema_version"`
	Type          OutboundType        `json:"type" yaml:"type"`
	ID            string              `json:"id,omitempty" yaml:"id,omitempty"`
	RequestID     string              `json:"request_id,omitempty" yaml:"request_id,omitempty"`
	RunID         string              `json:"run_id,omitempty" yaml:"run_id,omitempty"`
	Time          time.Time           `json:"time" yaml:"time"`
	Source        *InboundContext     `json:"source,omitempty" yaml:"source,omitempty"`
	Target        *InboundContext     `json:"target,omitempty" yaml:"target,omitempty"`
	Recipient     *OutboundRecipient  `json:"recipient,omitempty" yaml:"recipient,omitempty"`
	Correlation   OutboundCorrelation `json:"correlation,omitempty" yaml:"correlation,omitempty"`
	Data          map[string]any      `json:"data,omitempty" yaml:"data,omitempty"`
}

func NewOutboundEnvelope(outboundType OutboundType) OutboundEnvelope {
	return OutboundEnvelope{
		SchemaVersion: ContractSchemaVersion,
		Type:          outboundType,
		Time:          time.Now().UTC(),
	}
}

// Normalize trims wire-shape fields and fills transport-neutral defaults. It
// does not reject unknown outbound types; callers can use OutboundType.IsValid
// when they need strict value validation at an adapter boundary.
func (msg OutboundEnvelope) Normalize() OutboundEnvelope {
	if strings.TrimSpace(msg.SchemaVersion) == "" {
		msg.SchemaVersion = ContractSchemaVersion
	} else {
		msg.SchemaVersion = strings.TrimSpace(msg.SchemaVersion)
	}
	msg.Type = OutboundType(strings.TrimSpace(string(msg.Type)))
	msg.ID = strings.TrimSpace(msg.ID)
	msg.RequestID = strings.TrimSpace(msg.RequestID)
	msg.RunID = strings.TrimSpace(msg.RunID)
	if msg.Time.IsZero() {
		msg.Time = time.Now().UTC()
	} else {
		msg.Time = msg.Time.UTC()
	}
	msg.Source = normalizeOptionalInboundContext(msg.Source)
	msg.Target = normalizeOptionalInboundContext(msg.Target)
	if msg.Recipient != nil {
		recipient := msg.Recipient.Normalize()
		if recipient.ID == "" && recipient.IDType == "" {
			msg.Recipient = nil
		} else {
			msg.Recipient = &recipient
		}
	}
	msg.Correlation = msg.Correlation.Normalize()
	msg.Data = copyAnyMap(msg.Data)
	return msg
}

type OutboundEmitter interface {
	Capabilities() CapabilitySet
	Emit(context.Context, OutboundEnvelope) error
}

// copyAnyMap makes a shallow copy, trims keys, and gives exact untrimmed keys
// precedence over padded duplicates after trimming.
func copyAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed == key {
			out[key] = in[key]
		}
	}
	for _, key := range keys {
		trimmed := strings.TrimSpace(key)
		if trimmed == key {
			continue
		}
		if _, ok := out[trimmed]; ok {
			continue
		}
		out[trimmed] = in[key]
	}
	return out
}

func normalizeOptionalInboundContext(ctx *InboundContext) *InboundContext {
	if ctx == nil || !inboundContextHasAddress(*ctx) {
		return nil
	}
	normalized := NormalizeInboundContext(*ctx)
	return &normalized
}

func inboundContextHasAddress(ctx InboundContext) bool {
	return strings.TrimSpace(ctx.Channel) != "" ||
		strings.TrimSpace(ctx.EntrypointID) != "" ||
		strings.TrimSpace(ctx.Account) != "" ||
		strings.TrimSpace(ctx.ChannelAppID) != "" ||
		strings.TrimSpace(ctx.BotID) != "" ||
		strings.TrimSpace(ctx.ChatID) != "" ||
		strings.TrimSpace(ctx.TopicID) != "" ||
		strings.TrimSpace(ctx.SpaceID) != "" ||
		strings.TrimSpace(ctx.SenderID) != "" ||
		strings.TrimSpace(ctx.MessageID) != "" ||
		strings.TrimSpace(ctx.ReplyToMessageID) != "" ||
		strings.TrimSpace(ctx.ReplyToSenderID) != ""
}
