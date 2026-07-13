package runtime

import (
	"context"
	"encoding/json"
	"io"
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

func TestE2EOwnerTextResponseResumesAndDeliversFinalToOrigin(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if strings.Contains(lastUserMessage(request.Messages), "Use Tuesday") {
			return deepSeekHTTPResponse(deepSeekTextResponse("Owner selected Tuesday.")), nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("owner-text-call", "human_request", map[string]any{
				"kind": "freeform", "question": "Which deployment window?", "responder": "owner",
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	const entrypointID = "ilink-owner"
	rt.entrypoints = entrypoints.NewRegistry(agents.DefaultAgentID, []entrypoints.Definition{{ID: entrypointID, Channel: "ilink", Account: "acct-1"}})
	rt.ownerBindings.Set(ownerBinding{EntrypointID: entrypointID, OwnerSenderID: "owner-1", OwnerSenderIDType: "ilink_user_id"})
	outbound := &fakeHumanRequestOutbound{receiptID: "ilink-request-1"}
	rt.SetOutboundEmitter(outbound)
	origin := channel.InboundContext{
		Channel: "ilink", EntrypointID: entrypointID, Account: "acct-1", ChatID: "origin-group", ChatType: "group",
		SenderID: "coworker-1", SenderIDType: "ilink_user_id",
		Raw: map[string]string{"chat_id": "origin-group", "chat_type": "group", "sender_id_type": "ilink_user_id", "context_token": "origin-token"},
	}
	response, err := rt.RunAgent(context.Background(), TurnRequest{EntrypointID: entrypointID, Message: "ask the owner", Context: origin})
	if err != nil || response.Status != StatusWaitingHuman || len(response.HumanRequests) != 1 {
		t.Fatalf("initial owner request = %+v, %v", response, err)
	}
	request := response.HumanRequests[0]
	if request.Responder.Type != humanrequest.ResponderOwner || request.Delivery.Status != humanrequest.DeliverySent {
		t.Fatalf("owner request contract = %+v", request)
	}
	rendered, err := humanrequest.RenderTextRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	command, recognized, err := humanrequest.ParseTextResponse(strings.Replace(rendered[strings.LastIndex(rendered, "/answer"):], "<回答>", "Use Tuesday", 1))
	if err != nil || !recognized {
		t.Fatalf("rendered response command = %+v, %v, %v", command, recognized, err)
	}
	resolved, err := rt.ResolveHumanTextResponse(context.Background(), humanrequest.TextResponseEnvelope{
		CorrelationToken: command.CorrelationToken, EntrypointID: entrypointID,
		SenderID: "owner-1", SenderIDType: "ilink_user_id",
		ChatKey:  ChatKey{Channel: "ilink", ChatID: "owner-1", SenderID: "owner-1"}.String(),
		ChatType: "direct", Answer: command.Answer, IdempotencyKey: "ilink-owner-answer-1",
	})
	if err != nil || resolved.Resume.Status != humanrequest.ResumeCompleted {
		t.Fatalf("owner text resolve = %+v, %v", resolved, err)
	}
	if len(outbound.emitted) != 1 {
		t.Fatalf("resume final emissions = %+v", outbound.emitted)
	}
	target := outbound.emitted[0].Target
	if target == nil || target.Channel != "ilink" || target.ChatID != "origin-group" || target.SenderID != "coworker-1" {
		t.Fatalf("resume final target = %+v, want original group/coworker", target)
	}
	if target.ChatID == "owner-1" {
		t.Fatalf("owner response DM replaced persisted origin: %+v", target)
	}
}
