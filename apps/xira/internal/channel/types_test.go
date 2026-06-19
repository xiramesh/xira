package channel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewInboundContextNormalizesChannelFacts(t *testing.T) {
	ctx := NewInboundContext("Feishu", "user-1", map[string]string{
		"entrypoint_id": "feishu-expense-bot",
		"account":       "tenant-a",
		"app_id":        "cli-expense",
		"bot_id":        "bot-expense",
		"chat_id":       "chat-1",
		"chat_type":     "group",
		"topic_id":      "topic-1",
	})

	if ctx.Channel != "feishu" {
		t.Fatalf("channel = %q, want feishu", ctx.Channel)
	}
	if ctx.EntrypointID != "feishu-expense-bot" || ctx.Account != "tenant-a" || ctx.ChatID != "chat-1" || ctx.ChatType != "group" {
		t.Fatalf("context not populated from metadata: %+v", ctx)
	}
	if ctx.ChannelAppID != "cli-expense" || ctx.BotID != "bot-expense" {
		t.Fatalf("app/bot mismatch: %+v", ctx)
	}
	if ctx.SenderID != "user-1" || ctx.TopicID != "topic-1" {
		t.Fatalf("sender/topic mismatch: %+v", ctx)
	}
}

func TestOutboundEnvelopeNormalizesContractFields(t *testing.T) {
	msg := NewOutboundEnvelope(OutboundAssistantFinal)
	msg.ID = " out-1 "
	msg.RequestID = " req-1 "
	msg.RunID = " run-1 "
	msg.Source = &InboundContext{Channel: "XiraGarden", ChatID: " chat-1 ", SenderID: " user-1 "}
	msg.Target = &InboundContext{Channel: "Feishu", ChatID: " oc-1 ", SenderID: " xira "}
	msg.Data = map[string]any{
		" content ": "done",
	}

	normalized := msg.Normalize()

	if normalized.SchemaVersion != ContractSchemaVersion || normalized.Type != OutboundAssistantFinal {
		t.Fatalf("contract identity = %q/%q", normalized.SchemaVersion, normalized.Type)
	}
	if normalized.ID != "out-1" || normalized.RequestID != "req-1" || normalized.RunID != "run-1" {
		t.Fatalf("ids not normalized: %+v", normalized)
	}
	if normalized.Time.IsZero() {
		t.Fatal("time should be populated")
	}
	if normalized.Source == nil || normalized.Source.Channel != "xiragarden" || normalized.Source.ChatID != "chat-1" || normalized.Source.SenderID != "user-1" {
		t.Fatalf("source not normalized: %+v", normalized.Source)
	}
	if normalized.Target == nil || normalized.Target.Channel != "feishu" || normalized.Target.ChatID != "oc-1" || normalized.Target.SenderID != "xira" {
		t.Fatalf("target not normalized: %+v", normalized.Target)
	}
	if normalized.Data["content"] != "done" {
		t.Fatalf("data not normalized: %+v", normalized.Data)
	}
	if _, ok := msg.Data["content"]; ok {
		t.Fatalf("Normalize mutated original data map: %+v", msg.Data)
	}
}

func TestOutboundEnvelopeKeepsOptionalContextsEmpty(t *testing.T) {
	msg := NewOutboundEnvelope(OutboundProactiveMessage)
	msg.Source = &InboundContext{Channel: " ", ChatID: " ", SenderID: " "}

	normalized := msg.Normalize()

	if normalized.Source != nil {
		t.Fatalf("source should stay empty, got %+v", normalized.Source)
	}
	if normalized.Target != nil {
		t.Fatalf("target should stay empty, got %+v", normalized.Target)
	}
}

func TestOutboundEnvelopeOmitsEmptyOptionalContextsFromJSON(t *testing.T) {
	encoded, err := json.Marshal(NewOutboundEnvelope(OutboundAssistantFinal).Normalize())
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	if strings.Contains(body, `"source"`) || strings.Contains(body, `"target"`) {
		t.Fatalf("empty optional contexts should be omitted: %s", body)
	}
	if !strings.Contains(body, `"time"`) {
		t.Fatalf("time should be encoded: %s", body)
	}
}

func TestNormalizeCapabilitiesDedupesAndTrims(t *testing.T) {
	capabilities := NormalizeCapabilities([]Capability{
		Capability(" " + string(CapabilityStreamingDelta) + " "),
		CapabilityStreamingDelta,
		"",
		CapabilityProactiveOutbound,
	})

	if len(capabilities) != 2 {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if !capabilities.Supports(CapabilityStreamingDelta) || !capabilities.Supports(CapabilityProactiveOutbound) {
		t.Fatalf("missing expected capabilities: %+v", capabilities)
	}
	if capabilities.Supports(CapabilityOfflineQueue) {
		t.Fatalf("unexpected offline queue capability: %+v", capabilities)
	}
}
