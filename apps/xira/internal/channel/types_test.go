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

func TestNormalizeInboundContextPreservesAddressingFacts(t *testing.T) {
	addressed := []AddressTarget{AddressTargetOwner, AddressTargetAgent, AddressTargetOwner, AddressTarget("unknown")}
	mentions := []MentionTarget{
		{Key: " @_user_1 ", ID: " u_owner ", IDType: " user_id ", Name: " Owner "},
		{Key: "@_user_1", ID: "u_owner", IDType: "user_id", Name: "duplicate"},
		{Key: "@_user_2", ID: "u_owner", IDType: "user_id", Name: "Owner again"},
		{ID: " ", IDType: "open_id", Name: "empty"},
	}
	ctx := NormalizeInboundContext(InboundContext{
		Channel:        "feishu",
		ChatID:         "oc_group",
		SenderID:       "u_sender",
		AddressedTo:    addressed,
		MentionTargets: mentions,
	})

	if len(ctx.AddressedTo) != 2 || ctx.AddressedTo[0] != AddressTargetAgent || ctx.AddressedTo[1] != AddressTargetOwner {
		t.Fatalf("AddressedTo = %v, want [agent owner]", ctx.AddressedTo)
	}
	if len(ctx.MentionTargets) != 2 {
		t.Fatalf("MentionTargets = %+v, want one mapping per placeholder key", ctx.MentionTargets)
	}
	wantMention := (MentionTarget{Key: "@_user_1", ID: "u_owner", IDType: "user_id", Name: "Owner"})
	if ctx.MentionTargets[0] != wantMention {
		t.Fatalf("MentionTargets[0] = %+v, want %+v", ctx.MentionTargets[0], wantMention)
	}
	if ctx.MentionTargets[1].Key != "@_user_2" || ctx.MentionTargets[1].ID != "u_owner" {
		t.Fatalf("MentionTargets[1] = %+v, want second placeholder for same identity", ctx.MentionTargets[1])
	}
	if ctx.Raw["addressed_to"] != "agent,owner" {
		t.Fatalf("raw addressed_to = %q, want agent,owner", ctx.Raw["addressed_to"])
	}

	addressed[0] = AddressTargetAgent
	mentions[0].ID = "mutated"
	if ctx.AddressedTo[1] != AddressTargetOwner || ctx.MentionTargets[0].ID != "u_owner" {
		t.Fatalf("NormalizeInboundContext aliased caller slices: addressed=%v mentions=%+v", ctx.AddressedTo, ctx.MentionTargets)
	}
}

func TestNormalizeInboundContextPreservesSenderIDTypeRoundTrip(t *testing.T) {
	ctx := NormalizeInboundContext(InboundContext{
		Channel:      "feishu",
		ChatID:       "oc_group",
		SenderID:     "ou_sender",
		SenderIDType: " Open_ID ",
	})
	if ctx.SenderIDType != "open_id" {
		t.Fatalf("SenderIDType = %q, want open_id", ctx.SenderIDType)
	}
	if ctx.Raw["sender_id_type"] != "open_id" {
		t.Fatalf("raw sender_id_type = %q, want open_id", ctx.Raw["sender_id_type"])
	}

	restored := NormalizeInboundContext(InboundContext{
		Channel:  "feishu",
		ChatID:   "oc_group",
		SenderID: "ou_sender",
		Raw:      ctx.Raw,
	})
	if restored.SenderIDType != "open_id" {
		t.Fatalf("restored SenderIDType = %q, want open_id", restored.SenderIDType)
	}
}

func TestNormalizeInboundContextRestoresAddressingFactsFromRaw(t *testing.T) {
	ctx := NormalizeInboundContext(InboundContext{
		Channel:  "feishu",
		ChatID:   "oc_group",
		SenderID: "u_sender",
		Raw: map[string]string{
			"addressed_to": " owner, unknown, agent, owner ",
		},
	})

	if len(ctx.AddressedTo) != 2 || ctx.AddressedTo[0] != AddressTargetAgent || ctx.AddressedTo[1] != AddressTargetOwner {
		t.Fatalf("AddressedTo = %v, want [agent owner]", ctx.AddressedTo)
	}
	if ctx.Raw["addressed_to"] != "agent,owner" {
		t.Fatalf("raw addressed_to = %q, want normalized agent,owner", ctx.Raw["addressed_to"])
	}
}

func TestNormalizeInboundContextDropsUnknownAddressingFacts(t *testing.T) {
	ctx := NormalizeInboundContext(InboundContext{
		AddressedTo: []AddressTarget{"unknown", " "},
		Raw:         map[string]string{"addressed_to": "unknown"},
	})

	if len(ctx.AddressedTo) != 0 {
		t.Fatalf("AddressedTo = %v, want empty", ctx.AddressedTo)
	}
	if _, ok := ctx.Raw["addressed_to"]; ok {
		t.Fatalf("unknown addressed_to must not remain authoritative: %+v", ctx.Raw)
	}
}

func TestOutboundEnvelopeNormalizesContractFields(t *testing.T) {
	msg := NewOutboundEnvelope(OutboundAssistantFinal)
	msg.ID = " out-1 "
	msg.RequestID = " req-1 "
	msg.RunID = " run-1 "
	msg.Source = &InboundContext{Channel: "WebSocket", ChatID: " chat-1 ", SenderID: " user-1 "}
	msg.Target = &InboundContext{Channel: "Feishu", ChatID: " oc-1 ", SenderID: " xira "}
	msg.Recipient = &OutboundRecipient{ID: " ou_owner ", IDType: " OPEN_ID "}
	msg.Correlation = OutboundCorrelation{
		TraceID:         " trace-1 ",
		ParentRunID:     " parent-run ",
		ChildRunID:      " child-run ",
		ParentEventID:   " parent-event ",
		ToolCallID:      " tool-call ",
		ParentMessageID: " parent-message ",
	}
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
	if normalized.Recipient == nil || normalized.Recipient.ID != "ou_owner" || normalized.Recipient.IDType != "open_id" {
		t.Fatalf("recipient not normalized: %+v", normalized.Recipient)
	}
	if normalized.Source == nil || normalized.Source.Channel != "websocket" || normalized.Source.ChatID != "chat-1" || normalized.Source.SenderID != "user-1" {
		t.Fatalf("source not normalized: %+v", normalized.Source)
	}
	if normalized.Target == nil || normalized.Target.Channel != "feishu" || normalized.Target.ChatID != "oc-1" || normalized.Target.SenderID != "xira" {
		t.Fatalf("target not normalized: %+v", normalized.Target)
	}
	if normalized.Correlation.TraceID != "trace-1" ||
		normalized.Correlation.ParentRunID != "parent-run" ||
		normalized.Correlation.ChildRunID != "child-run" ||
		normalized.Correlation.ParentEventID != "parent-event" ||
		normalized.Correlation.ToolCallID != "tool-call" ||
		normalized.Correlation.ParentMessageID != "parent-message" {
		t.Fatalf("correlation not normalized: %+v", normalized.Correlation)
	}
	if normalized.Data["content"] != "done" {
		t.Fatalf("data not normalized: %+v", normalized.Data)
	}
	if _, ok := msg.Data["content"]; ok {
		t.Fatalf("Normalize mutated original data map: %+v", msg.Data)
	}
}

func TestOutboundEnvelopeDropsEmptyRecipient(t *testing.T) {
	env := NewOutboundEnvelope(OutboundProactiveMessage)
	env.Recipient = &OutboundRecipient{ID: " ", IDType: " "}
	if got := env.Normalize().Recipient; got != nil {
		t.Fatalf("empty recipient = %+v, want nil", got)
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

func TestOutboundEnvelopeDropsSupplementOnlyContexts(t *testing.T) {
	for name, ctx := range map[string]InboundContext{
		"mentioned": {Mentioned: true},
		"raw":       {Raw: map[string]string{"context_token": "token-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			msg := NewOutboundEnvelope(OutboundProactiveMessage)
			msg.Target = &ctx

			normalized := msg.Normalize()

			if normalized.Target != nil {
				t.Fatalf("supplement-only target should be omitted, got %+v", normalized.Target)
			}
		})
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

func TestOutboundTypeValidity(t *testing.T) {
	validTypes := []OutboundType{
		OutboundAck,
		OutboundRuntimeEvent,
		OutboundAssistantDelta,
		OutboundAssistantFinal,
		OutboundInterrupt,
		OutboundProactiveMessage,
		OutboundError,
	}
	for _, outboundType := range validTypes {
		if !outboundType.IsValid() {
			t.Fatalf("%q should be valid", outboundType)
		}
	}
	if OutboundType("unknown").IsValid() {
		t.Fatal("unknown outbound type should be invalid")
	}
	if NewOutboundEnvelope(OutboundType(" unknown ")).Normalize().Type != OutboundType("unknown") {
		t.Fatal("Normalize should trim but not reject unknown outbound types")
	}
}

func TestOutboundEnvelopeDataCopyIsShallowAndPrefersExactKeys(t *testing.T) {
	nested := map[string]any{"state": "shared"}
	msg := NewOutboundEnvelope(OutboundAssistantFinal)
	msg.Data = map[string]any{
		" content ": "padded",
		"content":   "exact",
		" nested ":  nested,
	}

	normalized := msg.Normalize()

	if normalized.Data["content"] != "exact" {
		t.Fatalf("exact key should win over padded duplicate: %+v", normalized.Data)
	}
	nestedCopy, ok := normalized.Data["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested value should be preserved: %+v", normalized.Data)
	}
	nestedCopy["state"] = "changed"
	if nested["state"] != "changed" {
		t.Fatalf("data copy should be shallow: %+v", normalized.Data)
	}
	normalized.Data["new"] = "value"
	if _, ok := msg.Data["new"]; ok {
		t.Fatalf("top-level data map should be copied: %+v", msg.Data)
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
