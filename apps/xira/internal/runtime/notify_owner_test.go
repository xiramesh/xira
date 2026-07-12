package runtime

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

func notifyOwnerTestService(t *testing.T, def entrypoints.Definition, binding ownerBinding, emitter channel.OutboundEmitter) *Service {
	t.Helper()
	store := newOwnerBindingStore(t.TempDir())
	if binding.EntrypointID != "" {
		store.Set(binding)
	}
	return &Service{
		entrypoints:   entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{def}),
		ownerBindings: store,
		outbound:      emitter,
	}
}

func TestResolveOwnerTargetUsesTypedDynamicBinding(t *testing.T) {
	svc := notifyOwnerTestService(t, entrypoints.Definition{
		ID: "feishu-owner", Channel: "feishu", Account: "tenant-a", AppID: "cli-a", BotID: "bot-a",
	}, ownerBinding{
		EntrypointID:      "feishu-owner",
		OwnerSenderID:     "ou_owner",
		OwnerSenderIDType: "open_id",
		BoundAt:           time.Now(),
	}, nil)

	target, err := svc.ResolveOwnerTarget(context.Background(), "feishu-owner")
	if err != nil {
		t.Fatalf("ResolveOwnerTarget: %v", err)
	}
	if target.Route.EntrypointID != "feishu-owner" || target.Route.Channel != "feishu" || target.Route.Account != "tenant-a" || target.Route.ChannelAppID != "cli-a" || target.Route.BotID != "bot-a" {
		t.Fatalf("route = %+v", target.Route)
	}
	if target.Recipient.ID != "ou_owner" || target.Recipient.IDType != "open_id" {
		t.Fatalf("recipient = %+v", target.Recipient)
	}
}

func TestResolveOwnerTargetFailsClosedForLegacyBinding(t *testing.T) {
	svc := notifyOwnerTestService(t, entrypoints.Definition{ID: "feishu-owner", Channel: "feishu"}, ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", BoundAt: time.Now(),
	}, nil)

	_, err := svc.ResolveOwnerTarget(context.Background(), "feishu-owner")
	if err == nil || !strings.Contains(err.Error(), "id type") {
		t.Fatalf("legacy binding error = %v, want missing id type", err)
	}
}

func TestResolveOwnerTargetUsesTypedStaticOwner(t *testing.T) {
	svc := notifyOwnerTestService(t, entrypoints.Definition{
		ID: "feishu-static", Channel: "feishu", OwnerID: "u_owner", OwnerIDType: "user_id",
	}, ownerBinding{}, nil)

	target, err := svc.ResolveOwnerTarget(context.Background(), "feishu-static")
	if err != nil {
		t.Fatalf("ResolveOwnerTarget: %v", err)
	}
	if target.Recipient != (channel.OutboundRecipient{ID: "u_owner", IDType: "user_id"}) {
		t.Fatalf("recipient = %+v", target.Recipient)
	}
}

func TestResolveOwnerTargetRejectsInvalidResolverInputs(t *testing.T) {
	var nilService *Service
	if _, err := nilService.ResolveOwnerTarget(context.Background(), "ep"); err == nil {
		t.Fatal("nil service must reject")
	}
	svc := notifyOwnerTestService(t, entrypoints.Definition{ID: "ep", Channel: "feishu"}, ownerBinding{}, nil)
	for _, tc := range []struct {
		entrypointID string
		want         string
	}{
		{"", "required"},
		{"missing", "not found"},
		{"ep", "no owner"},
	} {
		if _, err := svc.ResolveOwnerTarget(context.Background(), tc.entrypointID); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("ResolveOwnerTarget(%q) error = %v, want %q", tc.entrypointID, err, tc.want)
		}
	}
}

func TestNotifyOwnerToolCallUsesAuthoritativeTarget(t *testing.T) {
	emitter := &recordingEmitter{}
	svc := notifyOwnerTestService(t, entrypoints.Definition{
		ID: "feishu-owner", Channel: "feishu", Account: "tenant-a", AppID: "cli-a",
	}, ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	}, emitter)
	exec := runExecutionContext{
		Base: runtimeEventBase{RunID: "run-1", EntrypointID: "feishu-owner", Channel: "feishu", TraceID: "trace-1"},
		Request: TurnRequest{Context: channel.InboundContext{
			Channel: "feishu", EntrypointID: "feishu-owner", ChatID: "oc_group", SenderID: "ou_colleague",
		}},
	}
	ctx := contextWithRunExecution(context.Background(), exec)
	rec := svc.notifyOwnerToolCall(ctx, "call-1", map[string]any{
		"message":      "Please review the deployment decision.",
		"recipient_id": "ou_attacker",
	})

	if rec.Error != "" || rec.Output["status"] != "sent" {
		t.Fatalf("record = %+v", rec)
	}
	calls := emitter.emitted()
	if len(calls) != 1 {
		t.Fatalf("emitted %d envelopes, want 1", len(calls))
	}
	env := calls[0]
	if env.Type != channel.OutboundProactiveMessage || env.Target == nil || env.Target.EntrypointID != "feishu-owner" {
		t.Fatalf("envelope route = %+v", env)
	}
	if env.Recipient == nil || env.Recipient.ID != "ou_owner" || env.Recipient.IDType != "open_id" {
		t.Fatalf("model overrode or lost recipient: %+v", env.Recipient)
	}
	if env.Data["content"] != "Please review the deployment decision." {
		t.Fatalf("content = %#v", env.Data["content"])
	}
	if _, leaked := rec.Input["message"]; leaked {
		t.Fatalf("tool record leaked private message: %+v", rec.Input)
	}
}

func TestNotifyOwnerToolCallRejectsInvalidContextAndMessage(t *testing.T) {
	svc := &Service{}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		args map[string]any
		want string
	}{
		{name: "no execution", ctx: context.Background(), args: map[string]any{"message": "hello"}, want: "execution context"},
		{name: "empty message", ctx: contextWithRunExecution(context.Background(), runExecutionContext{Base: runtimeEventBase{EntrypointID: "ep"}}), args: map[string]any{"message": "  "}, want: "message is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := svc.notifyOwnerToolCall(tc.ctx, "call", tc.args)
			if rec.Error == "" || !strings.Contains(rec.Error, tc.want) || rec.Output["status"] != "rejected" {
				t.Fatalf("record = %+v", rec)
			}
		})
	}
}

func TestNotifyOwnerToolCallReportsDeliveryFailure(t *testing.T) {
	emitter := &recordingEmitter{emitErr: errors.New("feishu unavailable")}
	svc := notifyOwnerTestService(t, entrypoints.Definition{ID: "feishu-owner", Channel: "feishu"}, ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	}, emitter)
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{Base: runtimeEventBase{EntrypointID: "feishu-owner"}})

	rec := svc.notifyOwnerToolCall(ctx, "call", map[string]any{"message": "hello"})
	if rec.Output["status"] != "failed" || rec.Error == "" || !strings.Contains(rec.Error, "feishu unavailable") {
		t.Fatalf("record = %+v", rec)
	}
}

type nonTypedRecipientEmitter struct{}

func (nonTypedRecipientEmitter) Capabilities() channel.CapabilitySet {
	return channel.CapabilitySet{channel.CapabilityProactiveOutbound}
}
func (nonTypedRecipientEmitter) Emit(context.Context, channel.OutboundEnvelope) error {
	return errors.New("Emit must not run without typed recipient capability")
}

func TestNotifyOwnerToolCallCoversValidationAndDeliveryGuards(t *testing.T) {
	baseDef := entrypoints.Definition{ID: "feishu-owner", Channel: "feishu"}
	binding := ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	}
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{
		Request: TurnRequest{Context: channel.InboundContext{EntrypointID: "feishu-owner"}},
	})

	withNoEmitter := notifyOwnerTestService(t, baseDef, binding, nil)
	if rec := withNoEmitter.notifyOwnerToolCall(ctx, "call", map[string]any{"message": "hello"}); rec.Output["status"] != "failed" || !strings.Contains(rec.Error, "not configured") {
		t.Fatalf("no-emitter record = %+v", rec)
	}

	withoutCapability := notifyOwnerTestService(t, baseDef, binding, nonTypedRecipientEmitter{})
	if rec := withoutCapability.notifyOwnerToolCall(ctx, "call", map[string]any{"message": "hello"}); rec.Output["status"] != "failed" || !strings.Contains(rec.Error, "typed recipient") {
		t.Fatalf("no-capability record = %+v", rec)
	}

	noOwner := notifyOwnerTestService(t, baseDef, ownerBinding{}, &recordingEmitter{})
	if rec := noOwner.notifyOwnerToolCall(ctx, "call", map[string]any{"message": "hello"}); rec.Output["status"] != "rejected" || !strings.Contains(rec.Error, "no owner") {
		t.Fatalf("no-owner record = %+v", rec)
	}

	if rec := withNoEmitter.notifyOwnerToolCall(ctx, "call", map[string]any{"message": strings.Repeat("界", notifyOwnerMaxMessageRunes+1)}); rec.Output["status"] != "rejected" || !strings.Contains(rec.Error, "exceeds") {
		t.Fatalf("oversized record = %+v", rec)
	}
}

func TestNotifyOwnerAllowsOnlyOneSuccessfulDeliveryPerRun(t *testing.T) {
	emitter := &recordingEmitter{}
	svc := notifyOwnerTestService(t, entrypoints.Definition{ID: "feishu-owner", Channel: "feishu"}, ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	}, emitter)
	base := contextWithRunExecution(context.Background(), runExecutionContext{Base: runtimeEventBase{EntrypointID: "feishu-owner"}})
	ctx := contextWithNotifyOwnerRunState(base)

	first := svc.notifyOwnerToolCall(ctx, "call-1", map[string]any{"message": "first"})
	second := svc.notifyOwnerToolCall(ctx, "call-2", map[string]any{"message": "second"})
	if first.Output["status"] != "sent" {
		t.Fatalf("first notification = %+v", first)
	}
	if second.Output["status"] != "rejected" || !strings.Contains(second.Error, "already sent") {
		t.Fatalf("second notification = %+v, want per-run rejection", second)
	}
	if got := len(emitter.emitted()); got != 1 {
		t.Fatalf("owner deliveries = %d, want 1", got)
	}

	// A new run receives a fresh limiter.
	third := svc.notifyOwnerToolCall(contextWithNotifyOwnerRunState(base), "call-3", map[string]any{"message": "next run"})
	if third.Output["status"] != "sent" || len(emitter.emitted()) != 2 {
		t.Fatalf("new-run notification = %+v deliveries=%d", third, len(emitter.emitted()))
	}
}

func TestNotifyOwnerFailedDeliveryCanRetryWithinRun(t *testing.T) {
	emitter := &recordingEmitter{emitErr: errors.New("temporary failure")}
	svc := notifyOwnerTestService(t, entrypoints.Definition{ID: "feishu-owner", Channel: "feishu"}, ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	}, emitter)
	base := contextWithRunExecution(context.Background(), runExecutionContext{Base: runtimeEventBase{EntrypointID: "feishu-owner"}})
	ctx := contextWithNotifyOwnerRunState(base)

	first := svc.notifyOwnerToolCall(ctx, "call-1", map[string]any{"message": "first"})
	if first.Output["status"] != "failed" {
		t.Fatalf("first notification = %+v, want failure", first)
	}
	emitter.mu.Lock()
	emitter.emitErr = nil
	emitter.mu.Unlock()
	retry := svc.notifyOwnerToolCall(ctx, "call-2", map[string]any{"message": "retry"})
	if retry.Output["status"] != "sent" {
		t.Fatalf("retry notification = %+v, want sent", retry)
	}
	if !hasSuccessfulNotifyOwner([]ToolCallRecord{first, retry}) {
		t.Fatal("failed delivery followed by a successful retry must authorize intentional silence")
	}
}

func TestNotifyOwnerRunLimitIsConcurrentSafe(t *testing.T) {
	emitter := &recordingEmitter{}
	svc := notifyOwnerTestService(t, entrypoints.Definition{ID: "feishu-owner", Channel: "feishu"}, ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	}, emitter)
	base := contextWithRunExecution(context.Background(), runExecutionContext{Base: runtimeEventBase{EntrypointID: "feishu-owner"}})
	ctx := contextWithNotifyOwnerRunState(base)

	const calls = 16
	start := make(chan struct{})
	results := make(chan ToolCallRecord, calls)
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- svc.notifyOwnerToolCall(ctx, "", map[string]any{"message": "notify"})
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	sent, rejected := 0, 0
	for rec := range results {
		switch rec.Output["status"] {
		case "sent":
			sent++
		case "rejected":
			rejected++
		}
	}
	if sent != 1 || rejected != calls-1 || len(emitter.emitted()) != 1 {
		t.Fatalf("sent=%d rejected=%d deliveries=%d", sent, rejected, len(emitter.emitted()))
	}
}

func TestNotifyOwnerToolCallGeneratesIDAndUsesRequestEntrypointFallback(t *testing.T) {
	emitter := &recordingEmitter{}
	svc := notifyOwnerTestService(t, entrypoints.Definition{ID: "feishu-owner", Channel: "feishu"}, ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	}, emitter)
	ctx := contextWithRunExecution(context.Background(), runExecutionContext{
		Request: TurnRequest{Context: channel.InboundContext{EntrypointID: "feishu-owner"}},
	})
	rec := svc.notifyOwnerToolCall(ctx, "", map[string]any{"message": "hello"})
	if rec.ID == "" || rec.Output["status"] != "sent" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestRecordNotifyOwnerOutcomeIncludesFailureWithoutMessage(t *testing.T) {
	rec := ToolCallRecord{
		ID: "call", Name: notifyOwnerToolName,
		Input:  map[string]any{"message_chars": 5},
		Output: map[string]any{"status": "failed", "entrypoint_id": "ep"},
		Error:  "send failed",
	}
	var eventPayload map[string]any
	var auditAllowed bool
	recordNotifyOwnerOutcome(rec,
		func(_, _, _ string, payload map[string]any) { eventPayload = payload },
		func(_, _ string, allowed bool, _ string, _ map[string]any) { auditAllowed = allowed },
	)
	if eventPayload["error"] != "send failed" || eventPayload["message_chars"] != 5 || auditAllowed {
		t.Fatalf("event=%+v auditAllowed=%v", eventPayload, auditAllowed)
	}
	if _, leaked := eventPayload["message"]; leaked {
		t.Fatalf("private message leaked into event: %+v", eventPayload)
	}
}

func TestNotifyOwnerRegisteredForProductionADKPathOnly(t *testing.T) {
	for _, def := range runtimeNativeToolDefinitions(agents.BuiltinXiraAssistant()) {
		if def.Function.Name == notifyOwnerToolName {
			t.Fatal("dead native definitions must not advertise notify_owner")
		}
	}

	svc := &Service{}
	tools, err := svc.runtimeADKTools(context.Background(), agents.BuiltinXiraAssistant(), func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	foundADK := false
	for _, tool := range tools {
		if tool.Name() == notifyOwnerToolName {
			foundADK = true
		}
	}
	if !foundADK {
		t.Fatal("ADK tools missing notify_owner")
	}
}

func TestADKDeepSeekNotifyOwnerRunsThroughAgentLoop(t *testing.T) {
	modelCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		if modelCalls == 1 {
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("notify-call-1", notifyOwnerToolName, map[string]any{
				"message": "Owner, deployment needs your review.",
			})), nil
		}
		return deepSeekHTTPResponse(`{"model":"deepseek-v4-flash","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null}}]}`), nil
	})}
	rt := newTestService(t, Config{
		StateDir:       t.TempDir(),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{
		ID: "feishu-owner", Channel: "feishu", DefaultAgentID: agents.DefaultAgentID,
	}})
	rt.ownerBindings.Set(ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	})
	emitter := &recordingEmitter{}
	rt.outbound = emitter

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		EntrypointID: "feishu-owner",
		Message:      "notify the owner privately",
		Context: channel.InboundContext{
			Channel: "feishu", EntrypointID: "feishu-owner", ChatID: "oc_group", ChatType: "group", SenderID: "ou_colleague",
		},
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if modelCalls != 2 || resp.Status != "completed" || resp.FinalResponse != "" {
		t.Fatalf("calls=%d response=%+v", modelCalls, resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != notifyOwnerToolName || resp.ToolCalls[0].Output["status"] != "sent" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if len(emitter.emitted()) != 1 {
		t.Fatalf("owner notifications = %d, want 1", len(emitter.emitted()))
	}
}

func TestADKNotifyOwnerFailedThenSentAllowsIntentionalSilence(t *testing.T) {
	emitter := &recordingEmitter{emitErr: errors.New("temporary network failure")}
	modelCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		switch modelCalls {
		case 1:
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("notify-1", notifyOwnerToolName, map[string]any{"message": "first attempt"})), nil
		case 2:
			emitter.mu.Lock()
			emitter.emitErr = nil
			emitter.mu.Unlock()
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("notify-2", notifyOwnerToolName, map[string]any{"message": "retry"})), nil
		default:
			return deepSeekHTTPResponse(`{"model":"deepseek-v4-flash","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null}}]}`), nil
		}
	})}
	rt := newTestService(t, Config{
		StateDir:       t.TempDir(),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{
		ID: "feishu-owner", Channel: "feishu", DefaultAgentID: agents.DefaultAgentID,
	}})
	rt.ownerBindings.Set(ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	})
	rt.outbound = emitter

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		EntrypointID: "feishu-owner",
		Message:      "retry owner notification and stay silent",
		Context: channel.InboundContext{
			Channel: "feishu", EntrypointID: "feishu-owner", ChatID: "oc_group", ChatType: "group", SenderID: "ou_colleague",
		},
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if modelCalls != 3 || resp.Status != "completed" || resp.FinalResponse != "" || resp.VerificationResult.Status != "passed" {
		t.Fatalf("calls=%d response=%+v", modelCalls, resp)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].Output["status"] != "failed" || resp.ToolCalls[1].Output["status"] != "sent" {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if len(emitter.emitted()) != 1 {
		t.Fatalf("owner deliveries = %d, want one successful retry", len(emitter.emitted()))
	}
}

func TestADKNotifyOwnerAllowsOnlyOneSuccessfulDeliveryPerRun(t *testing.T) {
	modelCalls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		switch modelCalls {
		case 1:
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("notify-1", notifyOwnerToolName, map[string]any{"message": "first"})), nil
		case 2:
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("notify-2", notifyOwnerToolName, map[string]any{"message": "second"})), nil
		default:
			return deepSeekHTTPResponse(`{"model":"deepseek-v4-flash","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"Owner notification handled."}}]}`), nil
		}
	})}
	rt := newTestService(t, Config{
		StateDir:       t.TempDir(),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{
		ID: "feishu-owner", Channel: "feishu", DefaultAgentID: agents.DefaultAgentID,
	}})
	rt.ownerBindings.Set(ownerBinding{
		EntrypointID: "feishu-owner", OwnerSenderID: "ou_owner", OwnerSenderIDType: "open_id", BoundAt: time.Now(),
	})
	emitter := &recordingEmitter{}
	rt.outbound = emitter

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		EntrypointID: "feishu-owner",
		Message:      "notify twice",
		Context: channel.InboundContext{
			Channel: "feishu", EntrypointID: "feishu-owner", ChatID: "oc_group", ChatType: "group", SenderID: "ou_colleague",
		},
	})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if modelCalls != 3 || resp.Status != "completed" || resp.FinalResponse != "Owner notification handled." {
		t.Fatalf("response = %+v", resp)
	}
	sent, rejected := 0, 0
	for _, rec := range resp.ToolCalls {
		switch rec.Output["status"] {
		case "sent":
			sent++
		case "rejected":
			rejected++
		}
	}
	if sent != 1 || rejected != 1 || len(emitter.emitted()) != 1 {
		t.Fatalf("sent=%d rejected=%d deliveries=%d records=%+v", sent, rejected, len(emitter.emitted()), resp.ToolCalls)
	}
}

func TestHasSuccessfulNotifyOwner(t *testing.T) {
	if hasSuccessfulNotifyOwner(nil) {
		t.Fatal("empty records must not authorize intentional silence")
	}
	if hasSuccessfulNotifyOwner([]ToolCallRecord{{Name: notifyOwnerToolName, Error: "send failed", Output: map[string]any{"status": "failed"}}}) {
		t.Fatal("failed notification must not authorize intentional silence")
	}
	if !hasSuccessfulNotifyOwner([]ToolCallRecord{{Name: notifyOwnerToolName, Output: map[string]any{"status": "sent"}}}) {
		t.Fatal("successful notification should authorize intentional silence")
	}
	if !hasSuccessfulNotifyOwner([]ToolCallRecord{
		{Name: notifyOwnerToolName, Error: "network unavailable", Output: map[string]any{"status": "failed"}},
		{Name: notifyOwnerToolName, Output: map[string]any{"status": "sent"}},
	}) {
		t.Fatal("failed notification followed by sent should authorize intentional silence")
	}
	if !hasSuccessfulNotifyOwner([]ToolCallRecord{
		{Name: notifyOwnerToolName, Error: "invalid message", Output: map[string]any{"status": "rejected"}},
		{Name: notifyOwnerToolName, Output: map[string]any{"status": "sent"}},
	}) {
		t.Fatal("rejected notification followed by sent should authorize intentional silence")
	}
	if !hasSuccessfulNotifyOwner([]ToolCallRecord{
		{Name: notifyOwnerToolName, Output: map[string]any{"status": "sent"}},
		{Name: notifyOwnerToolName, Error: "owner notification already sent", Output: map[string]any{"status": "rejected"}},
	}) {
		t.Fatal("a later rejected notification must not undo an earlier successful delivery")
	}
	if hasSuccessfulNotifyOwner([]ToolCallRecord{
		{Name: notifyOwnerToolName, Output: map[string]any{"status": "sent"}},
		{Name: "write_file", Error: "permission denied", Output: map[string]any{"status": "failed"}},
	}) {
		t.Fatal("a later tool failure must prevent intentional silence from hiding the failure")
	}
	if hasSuccessfulNotifyOwner([]ToolCallRecord{
		{Name: notifyOwnerToolName, Output: map[string]any{"status": "sent"}},
		{Name: "write_file", Output: map[string]any{"status": "rejected"}},
	}) {
		t.Fatal("a rejected tool without a Go error must still prevent intentional silence")
	}
}
