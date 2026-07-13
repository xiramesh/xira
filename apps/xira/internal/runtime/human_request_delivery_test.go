package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/entrypoints"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

type fakeHumanRequestOutbound struct {
	validateErr error
	deliverErr  error
	receiptID   string
	targets     []HumanRequestDeliveryTarget
	emitted     []channel.OutboundEnvelope
}

func (f *fakeHumanRequestOutbound) Capabilities() channel.CapabilitySet { return nil }

func (f *fakeHumanRequestOutbound) Emit(_ context.Context, env channel.OutboundEnvelope) error {
	f.emitted = append(f.emitted, env)
	return nil
}
func (f *fakeHumanRequestOutbound) ValidateHumanRequestDelivery(target HumanRequestDeliveryTarget) error {
	f.targets = append(f.targets, target)
	return f.validateErr
}
func (f *fakeHumanRequestOutbound) DeliverHumanRequest(_ context.Context, _ humanrequest.HumanRequest, target HumanRequestDeliveryTarget) (HumanRequestDeliveryReceipt, error) {
	f.targets = append(f.targets, target)
	if f.deliverErr != nil {
		return HumanRequestDeliveryReceipt{}, f.deliverErr
	}
	return HumanRequestDeliveryReceipt{MessageID: f.receiptID}, nil
}

func TestCreateAgentHumanRequestBindsAndDeliversOwnerPrivately(t *testing.T) {
	rt, baseCtx := newHumanRequestToolTestRuntime(t, "run-owner-delivery", "session-owner-delivery")
	entrypointID := "ilink-owner"
	rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{ID: entrypointID, Channel: "ilink", Account: "acct-1"}})
	rt.ownerBindings.Set(ownerBinding{EntrypointID: entrypointID, OwnerSenderID: "owner-1", OwnerSenderIDType: "ilink_user_id"})
	outbound := &fakeHumanRequestOutbound{receiptID: "ilink-message-owner"}
	rt.SetOutboundEmitter(outbound)
	exec, _ := runExecutionFromContext(baseCtx)
	exec.Base.EntrypointID = entrypointID
	exec.Request.Context = channel.InboundContext{
		Channel: "ilink", EntrypointID: entrypointID, Account: "acct-1",
		ChatID: "origin-group", SenderID: "coworker", SenderIDType: "ilink_user_id",
	}
	ctx := contextWithRunExecution(baseCtx, exec)
	req, err := rt.createAgentHumanRequest(ctx, "human-owner-delivery", map[string]any{
		"kind": "approval", "question": "Owner approve?", "responder": "owner",
	})
	if err != nil {
		t.Fatalf("createAgentHumanRequest() error = %v", err)
	}
	if req.Responder.Type != humanrequest.ResponderOwner || req.Responder.SenderID != "owner-1" || req.Responder.SenderIDType != "ilink_user_id" {
		t.Fatalf("owner responder = %+v", req.Responder)
	}
	if req.Delivery.Status != humanrequest.DeliverySent || req.Delivery.MessageID != "ilink-message-owner" {
		t.Fatalf("owner delivery = %+v", req.Delivery)
	}
	lastTarget := outbound.targets[len(outbound.targets)-1]
	if lastTarget.Recipient == nil || lastTarget.Recipient.ID != "owner-1" || lastTarget.Route.EntrypointID != entrypointID {
		t.Fatalf("owner delivery target = %+v", lastTarget)
	}
}

func TestCreateAgentHumanRequestRejectsUnreachableOwnerBeforePersist(t *testing.T) {
	rt, baseCtx := newHumanRequestToolTestRuntime(t, "run-owner-unreachable", "session-owner-unreachable")
	entrypointID := "ilink-owner"
	rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{ID: entrypointID, Channel: "ilink", Account: "acct-1"}})
	rt.ownerBindings.Set(ownerBinding{EntrypointID: entrypointID, OwnerSenderID: "owner-1", OwnerSenderIDType: "ilink_user_id"})
	exec, _ := runExecutionFromContext(baseCtx)
	exec.Base.EntrypointID = entrypointID
	exec.Request.Context = channel.InboundContext{Channel: "ilink", EntrypointID: entrypointID, SenderID: "coworker", SenderIDType: "ilink_user_id"}
	ctx := contextWithRunExecution(baseCtx, exec)
	if _, err := rt.createAgentHumanRequest(ctx, "human-owner-unreachable", map[string]any{
		"kind": "approval", "question": "Owner approve?", "responder": "owner",
	}); err == nil || !strings.Contains(err.Error(), "delivery") {
		t.Fatalf("unreachable owner error = %v", err)
	}
	pending, err := rt.ListHumanRequests(context.Background(), humanrequest.StatusPending)
	if err != nil || len(pending) != 0 {
		t.Fatalf("unreachable owner persisted requests = %+v, %v", pending, err)
	}
}

func TestCreateAgentHumanRequestFailsDeliveryWithoutSuspending(t *testing.T) {
	rt, baseCtx := newHumanRequestToolTestRuntime(t, "run-delivery-failed", "session-delivery-failed")
	entrypointID := "ilink-owner"
	rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{ID: entrypointID, Channel: "ilink", Account: "acct-1"}})
	rt.ownerBindings.Set(ownerBinding{EntrypointID: entrypointID, OwnerSenderID: "owner-1", OwnerSenderIDType: "ilink_user_id"})
	outbound := &fakeHumanRequestOutbound{deliverErr: errors.New("ilink unavailable")}
	rt.SetOutboundEmitter(outbound)
	exec, _ := runExecutionFromContext(baseCtx)
	exec.Base.EntrypointID = entrypointID
	exec.Request.Context = channel.InboundContext{
		Channel: "ilink", EntrypointID: entrypointID, Account: "acct-1",
		ChatID: "origin-group", ChatType: "group", SenderID: "coworker-1", SenderIDType: "ilink_user_id",
	}
	ctx := contextWithRunExecution(baseCtx, exec)
	req, err := rt.createAgentHumanRequest(ctx, "human-delivery-failed", map[string]any{
		"kind": "freeform", "question": "Which window?", "responder": "owner",
	})
	if err == nil || !strings.Contains(err.Error(), "ilink unavailable") {
		t.Fatalf("delivery failure error = %v", err)
	}
	collector := runtimeSuspendCollectorFromContext(ctx)
	if collector == nil || collector.Interrupt() != nil {
		t.Fatal("delivery failure must not leave the run waiting_human")
	}
	pending, listErr := rt.ListHumanRequests(context.Background(), humanrequest.StatusPending)
	if listErr != nil || len(pending) != 0 {
		t.Fatalf("delivery failure left pending requests = %+v, %v", pending, listErr)
	}
	failed, listErr := rt.ListHumanRequests(context.Background(), humanrequest.StatusFailed)
	if listErr != nil || len(failed) != 1 || failed[0].Delivery.Status != humanrequest.DeliveryFailed || failed[0].Delivery.LastError == "" {
		t.Fatalf("terminal failed requests = %+v, %v (returned=%+v)", failed, listErr, req)
	}
}

func TestRunAgentOwnerDeliveryFailureFailsRunInsteadOfHanging(t *testing.T) {
	var modelCalls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		modelCalls++
		var request deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if modelCalls == 1 {
			return deepSeekHTTPResponse(deepSeekToolCallResponseWithArgs("owner-delivery-fails", "human_request", map[string]any{
				"kind": "freeform", "question": "Owner decision?", "responder": "owner",
			})), nil
		}
		return deepSeekHTTPResponse(deepSeekTextResponse("unexpected second model call")), nil
	})}
	rt := newTestService(t, Config{
		StateDir: filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(
			deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client),
		),
	})
	const entrypointID = "ilink-owner"
	rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{ID: entrypointID, Channel: "ilink", Account: "acct-1"}})
	rt.ownerBindings.Set(ownerBinding{EntrypointID: entrypointID, OwnerSenderID: "owner-1", OwnerSenderIDType: "ilink_user_id"})
	rt.SetOutboundEmitter(&fakeHumanRequestOutbound{deliverErr: errors.New("private push unavailable")})
	response, err := rt.RunAgent(context.Background(), TurnRequest{
		EntrypointID: entrypointID, Message: "ask owner",
		Context: channel.InboundContext{Channel: "ilink", EntrypointID: entrypointID, Account: "acct-1", ChatID: "origin-group", ChatType: "group", SenderID: "coworker-1", SenderIDType: "ilink_user_id"},
	})
	if err == nil || !strings.Contains(err.Error(), "private push unavailable") || response.Status != "failed" || response.Interrupt != nil || len(response.HumanRequests) != 0 {
		t.Fatalf("RunAgent delivery failure response = %+v, %v", response, err)
	}
	if modelCalls != 1 {
		t.Fatalf("model calls = %d, want delivery failure to stop the run", modelCalls)
	}
	failed, err := rt.ListHumanRequests(context.Background(), humanrequest.StatusFailed)
	if err != nil || len(failed) != 1 || failed[0].Delivery.Status != humanrequest.DeliveryFailed {
		t.Fatalf("terminal owner delivery records = %+v, %v", failed, err)
	}
}

func TestHumanRequestToolSchemaSealsResponderChoice(t *testing.T) {
	schema := humanRequestToolInputSchema()
	responder := schema.Properties["responder"]
	if responder == nil || len(responder.Enum) != 2 || responder.Enum[0] != "current_sender" || responder.Enum[1] != "owner" {
		t.Fatalf("responder schema = %+v", responder)
	}
}

func TestPrepareHumanRequestInteractionSealsResponderAndRouteValidation(t *testing.T) {
	inbound := channel.InboundContext{Channel: "ilink", EntrypointID: "ilink-owner", Account: "acct-1", ChatID: "group-1", SenderID: "sender-1", SenderIDType: "ILINK_USER_ID"}

	t.Run("current sender falls back when delivery port is absent", func(t *testing.T) {
		rt := newTestService(t, Config{})
		policy, target, required, err := rt.prepareHumanRequestInteraction(context.Background(), inbound, "")
		if err != nil || required || policy.Type != humanrequest.ResponderCurrentSender || target.Route.SenderID != "sender-1" {
			t.Fatalf("prepare current sender = (%+v, %+v, %v, %v)", policy, target, required, err)
		}
	})

	t.Run("unsupported current sender delivery falls back", func(t *testing.T) {
		rt := newTestService(t, Config{})
		rt.SetOutboundEmitter(&fakeHumanRequestOutbound{validateErr: fmt.Errorf("wrapped: %w", ErrHumanRequestDeliveryUnsupported)})
		policy, _, required, err := rt.prepareHumanRequestInteraction(context.Background(), inbound, "current_sender")
		if err != nil || required || policy.Type != humanrequest.ResponderCurrentSender {
			t.Fatalf("prepare unsupported current sender = (%+v, %v, %v)", policy, required, err)
		}
	})

	t.Run("invalid current sender route fails", func(t *testing.T) {
		rt := newTestService(t, Config{})
		rt.SetOutboundEmitter(&fakeHumanRequestOutbound{validateErr: errors.New("bad route")})
		if _, _, _, err := rt.prepareHumanRequestInteraction(context.Background(), inbound, "current_sender"); err == nil || !strings.Contains(err.Error(), "current-sender") {
			t.Fatalf("current sender route error = %v", err)
		}
	})

	t.Run("owner resolution and validation fail closed", func(t *testing.T) {
		rt := newTestService(t, Config{})
		rt.SetOutboundEmitter(&fakeHumanRequestOutbound{})
		if _, _, _, err := rt.prepareHumanRequestInteraction(context.Background(), inbound, "owner"); err == nil || !strings.Contains(err.Error(), "resolve owner") {
			t.Fatalf("missing owner resolution error = %v", err)
		}

		rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{ID: "ilink-owner", Channel: "ilink", Account: "acct-1", OwnerID: "owner-1", OwnerIDType: "ilink_user_id"}})
		rt.SetOutboundEmitter(&fakeHumanRequestOutbound{validateErr: errors.New("private push disabled")})
		if _, _, _, err := rt.prepareHumanRequestInteraction(context.Background(), inbound, "owner"); err == nil || !strings.Contains(err.Error(), "validate owner") {
			t.Fatalf("owner delivery validation error = %v", err)
		}
	})

	t.Run("unknown responder is rejected", func(t *testing.T) {
		rt := newTestService(t, Config{})
		if _, _, _, err := rt.prepareHumanRequestInteraction(context.Background(), inbound, "anyone"); !errors.Is(err, humanrequest.ErrValidation) {
			t.Fatalf("unknown responder error = %v", err)
		}
	})
}

func TestCreateAndDeliverHumanRequestHandlesDeliveryEdgeCases(t *testing.T) {
	t.Run("non-delivered request stays compatible", func(t *testing.T) {
		rt := newTestService(t, Config{})
		req, err := rt.createAndDeliverHumanRequest(context.Background(), validHumanRequestCreate("no-delivery"), HumanRequestDeliveryTarget{}, false)
		if err != nil || req.Delivery.Status != humanrequest.DeliveryNone {
			t.Fatalf("non-delivered request = (%+v, %v)", req, err)
		}
	})

	t.Run("empty receipt is a durable delivery failure", func(t *testing.T) {
		rt := newTestService(t, Config{})
		rt.SetOutboundEmitter(&fakeHumanRequestOutbound{})
		req, err := rt.createAndDeliverHumanRequest(context.Background(), validHumanRequestCreate("empty-receipt"), HumanRequestDeliveryTarget{}, true)
		if err == nil || req.Status != humanrequest.StatusFailed || req.Delivery.Status != humanrequest.DeliveryFailed || !strings.Contains(req.Delivery.LastError, "empty message id") {
			t.Fatalf("empty receipt request = (%+v, %v)", req, err)
		}
	})

	t.Run("delivery disappearing after validation is explicit", func(t *testing.T) {
		rt := newTestService(t, Config{})
		if _, err := rt.createAndDeliverHumanRequest(context.Background(), validHumanRequestCreate("missing-port"), HumanRequestDeliveryTarget{}, true); err == nil || !strings.Contains(err.Error(), "became unavailable") {
			t.Fatalf("missing delivery port error = %v", err)
		}
	})

	t.Run("create validation error is returned", func(t *testing.T) {
		rt := newTestService(t, Config{})
		if _, err := rt.createAndDeliverHumanRequest(context.Background(), humanrequest.CreateRequest{}, HumanRequestDeliveryTarget{}, false); err == nil {
			t.Fatal("invalid create unexpectedly succeeded")
		}
	})
}

func validHumanRequestCreate(suffix string) humanrequest.CreateRequest {
	return humanrequest.CreateRequest{
		WorkspaceID: "workspace", RunID: "run-" + suffix, AgentID: "agent", SessionID: "session",
		Source: "agent_request", Kind: humanrequest.RequestFreeform, Question: "Question?",
		DedupeKey: "dedupe-" + suffix, Responder: currentSenderResponder(channel.InboundContext{SenderID: "sender-1", SenderIDType: "ilink_user_id"}),
	}
}
