package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	fsession "github.com/xiramesh/xira/internal/session"
)

// TestTurnRequestCarriesInboundContext asserts that TurnRequest carries session
// identity as a first-class channel.InboundContext, not as flattened
// channel/user_id/metadata fields. This is the unified identity contract: a
// single source of truth for "where this conversation came from".
func TestTurnRequestCarriesInboundContext(t *testing.T) {
	req := TurnRequest{
		AgentID: "expense-bot",
		Message: "hi",
		Context: channel.InboundContext{
			Channel:  "feishu",
			ChatID:   "oc_x",
			SenderID: "u_y",
			SpaceID:  "ti_demo",
		},
	}

	// Identity comes from Context, the one source of truth.
	if req.Context.Channel != "feishu" {
		t.Fatalf("context channel = %q, want feishu", req.Context.Channel)
	}
	if req.Context.ChatID != "oc_x" || req.Context.SenderID != "u_y" {
		t.Fatalf("context identity lost: %+v", req.Context)
	}

	// Wire format must nest identity under "context" and must NOT flatten it
	// into top-level channel/user_id/metadata keys.
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := wire["context"]; !ok {
		t.Fatalf("wire JSON missing nested context object: %s", data)
	}
	for _, stale := range []string{"channel", "user_id", "metadata"} {
		if _, ok := wire[stale]; ok {
			t.Fatalf("wire JSON still has flattened %q (should live under context): %s", stale, data)
		}
	}
	ctxMap, _ := wire["context"].(map[string]any)
	if ctxMap["channel"] != "feishu" || ctxMap["chat_id"] != "oc_x" || ctxMap["sender_id"] != "u_y" {
		t.Fatalf("nested context missing identity fields: %s", data)
	}
}

// TestTurnRequestDecodesNestedContext asserts the HTTP API wire shape: a body
// with a nested context object decodes into TurnRequest.Context. This pins the
// new endpoint contract (no compatibility with the old flattened fields).
func TestTurnRequestDecodesNestedContext(t *testing.T) {
	body := `{"message":"hi","agent_id":"expense-bot","context":{"channel":"feishu","chat_id":"oc_x","sender_id":"u_y","space_id":"ti_demo","chat_type":"group"}}`
	var req TurnRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode nested context body: %v", err)
	}
	if req.Message != "hi" || req.AgentID != "expense-bot" {
		t.Fatalf("message/agent lost: %+v", req)
	}
	if req.Context.Channel != "feishu" || req.Context.ChatID != "oc_x" || req.Context.SenderID != "u_y" || req.Context.SpaceID != "ti_demo" || req.Context.ChatType != "group" {
		t.Fatalf("context not decoded: %+v", req.Context)
	}
}

// TestRunAgentPersistsSessionInTriggerChannel is the behavioral core of the
// unified identity contract: a TurnRequest carrying InboundContext must cause
// the session to land under the trigger channel's directory tree (not a forged
// "flow"/"resume" channel), with the real chat/sender in the path.
//
// This runs against the fake DeepSeek client — session placement is determined
// entirely by InboundContext, independent of what the LLM returns.
func TestRunAgentPersistsSessionInTriggerChannel(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "hello",
		Context: channel.InboundContext{
			Channel:  "feishu",
			ChatID:   "oc_smoke",
			SenderID: "u_smoke",
			ChatType: "group",
		},
	})
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	if resp.Status != "completed" {
		t.Fatalf("status = %q, want completed", resp.Status)
	}
	// The session scope must reflect the trigger channel, not a forged one.
	if resp.SessionScope == nil {
		t.Fatal("session scope is nil")
	}
	if resp.SessionScope.Channel != "feishu" {
		t.Fatalf("session scope channel = %q, want feishu (trigger channel)", resp.SessionScope.Channel)
	}
	if got := resp.SessionScope.Values["chat"]; got != "group:oc_smoke" {
		t.Fatalf("session scope chat = %q, want group:oc_smoke", got)
	}
	if got := resp.SessionScope.Values["sender"]; !strings.Contains(got, "u_smoke") {
		t.Fatalf("session scope sender = %q, want it to contain u_smoke", got)
	}

	// And the messages.jsonl must physically land under sessions/feishu/...
	scope := resp.SessionScope
	msgPath := rt.SessionManager().AgentMessagesPath(fsession.AgentTurnInput{
		SessionID: resp.SessionID,
		AgentID:   resp.AgentID,
		Context: channel.InboundContext{
			Channel:      scope.Channel,
			EntrypointID: resp.EntrypointID,
			ChatID:       scopeChatID(scope.Values["chat"]),
			ChatType:     "group",
			SenderID:     scope.Values["sender"],
		},
		Scope: scope,
	})
	if msgPath == "" {
		t.Fatal("agent messages path is empty")
	}
	if !strings.Contains(filepath.ToSlash(msgPath), "/sessions/feishu/") {
		t.Fatalf("messages path not under sessions/feishu/: %s", msgPath)
	}
	if !strings.Contains(filepath.ToSlash(msgPath), "oc_smoke") {
		t.Fatalf("messages path missing real chat id oc_smoke: %s", msgPath)
	}
}
