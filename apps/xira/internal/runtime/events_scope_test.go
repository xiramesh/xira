package runtime

import (
	"testing"

	fsession "github.com/xiramesh/xira/internal/session"
)

// events_scope_test.go: tests inboundContextFromScope's field round-trip from a
// persisted SessionScope. Critical for the resume path (RFC #27 — stateless
// HITL resume): every field stored at scope-build time must be faithfully
// recoverable, or the resumed run's outbound delivery routes to the wrong
// target (PR #71 review CRITICAL: ilink sender carried an "ilink:" prefix →
// final delivered to a non-existent user).
//
// Why real canonical products (not hand-clean values): scope-build stores
// canonicalized sender ids (canonicalSenderID → "ilink:wxid_abc"). The
// reconstruction must strip that prefix symmetrically with chat/space/topic
// (which go through scopeValueID). Hand-crafted "sender": "wxid_abc" in tests
// bypasses canonicalSenderID and hides the asymmetry (PR #71's blind spot).

// buildScopeViaManager builds a SessionScope the way the session manager does:
// it canonicalizes the sender (adds "<channel>:" prefix) before storing. This
// produces the SAME shape of scope.Values["sender"] that a real persisted run
// carries — so the test exercises the actual transform, not a sanitized one.
//
// canonicalSenderID (session/manager.go:344) is unexported; its product with
// empty IdentityLinks is exactly "<channel>:<senderID>" (manager.go:358). We
// construct that product directly here rather than reaching into the session
// package — the point is to feed inboundContextFromScope the REAL persisted
// shape, not a hand-cleaned "wxid_abc".
func buildScopeViaManager(t *testing.T, ch, chatID, senderID string) *fsession.SessionScope {
	t.Helper()
	canonical := ch + ":" + senderID // mirrors canonicalSenderID(ch, senderID, nil)
	return &fsession.SessionScope{
		Channel:      ch,
		EntrypointID: "ep-1",
		Account:      "acct-1",
		Values: map[string]string{
			"chat":   "p2p:" + chatID,
			"sender": canonical,
		},
	}
}

// TestInboundContextFromScopeStripsSenderPrefix is the regression test for PR
// #71 CRITICAL: sender must be de-prefixed symmetrically with chat/space.
// Before the fix, ilink sender "ilink:wxid_abc" was carried verbatim into
// Target.SenderID, so resume Emit delivered to ToUserID="ilink:wxid_abc"
// (non-existent).
func TestInboundContextFromScopeStripsSenderPrefix(t *testing.T) {
	tests := []struct {
		name      string
		channel   string
		rawSender string // the original senderID before canonicalization
	}{
		{"ilink", "ilink", "wxid_abc"},
		{"feishu", "feishu", "ou_abcdef123"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scope := buildScopeViaManager(t, tc.channel, "chat-1", tc.rawSender)
			got := inboundContextFromScope(scope, nil)

			// chat is de-prefixed (existing behavior) — sanity check.
			if got.ChatID != "chat-1" {
				t.Errorf("ChatID = %q, want chat-1 (chat prefix must be stripped)", got.ChatID)
			}
			// sender MUST be de-prefixed too — this is the bug being fixed.
			if got.SenderID != tc.rawSender {
				t.Errorf("SenderID = %q, want %q (canonical %q prefix must be stripped, symmetric with chat)",
					got.SenderID, tc.rawSender, tc.channel+":")
			}
			if got.Channel != tc.channel {
				t.Errorf("Channel = %q, want %q", got.Channel, tc.channel)
			}
		})
	}
}

// TestInboundContextFromScopePreservesSenderWithoutPrefix verifies a sender
// that already has no canonical prefix (edge: legacy/foreign scope) is passed
// through unchanged — scopeValueID returns the input when there's no ":".
func TestInboundContextFromScopePreservesSenderWithoutPrefix(t *testing.T) {
	scope := &fsession.SessionScope{
		Channel: "ilink",
		Values:  map[string]string{"sender": "plain_id"},
	}
	got := inboundContextFromScope(scope, nil)
	if got.SenderID != "plain_id" {
		t.Errorf("SenderID = %q, want plain_id (no prefix → unchanged)", got.SenderID)
	}
}

// TestInboundContextFromScopeNilScopeRawOnly verifies the nil-scope fallback
// builds a context from raw metadata alone (used when a run has no scope).
func TestInboundContextFromScopeNilScopeRawOnly(t *testing.T) {
	raw := map[string]string{"context_token": "tok-1"}
	got := inboundContextFromScope(nil, raw)
	if got.Raw["context_token"] != "tok-1" {
		t.Errorf("raw context_token lost: %+v", got.Raw)
	}
}
