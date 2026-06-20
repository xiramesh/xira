package progress

import (
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/runtime"
)

func inboundFixture() channel.InboundContext {
	return channel.NormalizeInboundContext(channel.InboundContext{
		Channel:      "ilink",
		EntrypointID: "ep-1",
		ChatID:       "chat-1",
		SenderID:     "user-1",
		MessageID:    "msg-1",
	})
}

func evtWithScope(kind, runID string, scope runtime.RuntimeEventScope) runtime.RuntimeEvent {
	scope.RunID = runID
	return runtime.RuntimeEvent{
		ID:    "evt-" + kind,
		Kind:  kind,
		RunID: runID,
		Scope: &scope,
	}
}

func scopeFixture() runtime.RuntimeEventScope {
	return runtime.RuntimeEventScope{
		EntrypointID: "ep-1",
		Channel:      "ilink",
		ChatID:       "chat-1",
		SenderID:     "user-1",
		MessageID:    "msg-1",
	}
}

// TestScopeMatcherMatchesInboundScope: an event whose scope equals the inbound
// request scope matches and registers its run id.
func TestScopeMatcherMatchesInboundScope(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	evt := evtWithScope("agent.delegate.failed", "run-1", scopeFixture())
	if !m.match(evt) {
		t.Fatalf("event matching inbound scope should match")
	}
	// After run-1 is registered, a follow-up event carrying only the run id
	// (no scope) still matches.
	evt2 := runtime.RuntimeEvent{ID: "e2", Kind: "tool.completed", RunID: "run-1"}
	if !m.match(evt2) {
		t.Fatalf("event with registered run id should match")
	}
}

// TestScopeMatcherRequiresMessageIDHardIsolation: two consecutive direct-chat
// turns share EntrypointID+ChatID+SenderID; MessageID is the only separator
// before the run id is known. An event with a different MessageID must NOT
// match.
func TestScopeMatcherRequiresMessageIDHardIsolation(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	other := scopeFixture()
	other.MessageID = "msg-2" // different turn
	if m.match(evtWithScope("agent.delegate.timeout", "run-other", other)) {
		t.Fatalf("event with different MessageID must not match (cross-turn leak)")
	}
}

// TestScopeMatcherTracksChildRunViaCorrelation: once the parent run id is
// discovered from a scope match, child-run events correlated by parent_run_id
// / child_run_id / trace_id match even without a matching scope.
func TestScopeMatcherTracksChildRunViaCorrelation(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	// First, register parent run via scope match.
	if !m.match(evtWithScope("run.started", "parent-run", scopeFixture())) {
		t.Fatalf("run.started should match inbound scope")
	}
	cases := []struct {
		name string
		corr runtime.RuntimeEventCorrelation
	}{
		{"trace_id", runtime.RuntimeEventCorrelation{TraceID: "parent-run"}},
		{"parent_run_id", runtime.RuntimeEventCorrelation{ParentRunID: "parent-run"}},
	}
	for _, c := range cases {
		evt := runtime.RuntimeEvent{ID: "c-" + c.name, Kind: "adk.event", Correlation: &c.corr}
		if !m.match(evt) {
			t.Fatalf("child event matched by %s should match", c.name)
		}
	}
}

// TestScopeMatcherRejectsDifferentChat: an event from a different chat must not
// match, even with a populated scope.
func TestScopeMatcherRejectsDifferentChat(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	other := scopeFixture()
	other.ChatID = "chat-other"
	other.MessageID = "msg-1"
	if m.match(evtWithScope("run.waiting_human", "run-x", other)) {
		t.Fatalf("event from a different chat must not match")
	}
}

// TestScopeMatcherNoMessageIDDisablesMatching: when the inbound request carries
// no MessageID, scope matching is disabled (no safe turn isolation exists), so
// nothing matches — the forwarder must stay silent rather than risk leaking.
func TestScopeMatcherNoMessageIDDisablesMatching(t *testing.T) {
	noMsg := inboundFixture()
	noMsg.MessageID = ""
	m := newScopeMatcher(noMsg)
	sc := scopeFixture()
	sc.MessageID = ""
	if m.match(evtWithScope("agent.delegate.failed", "run-1", sc)) {
		t.Fatalf("without inbound MessageID, no event should match")
	}
}

// TestScopeMatcherRejectsDifferentAccount: iLink supports multiple accounts
// under one entrypoint, so two turns can share EntrypointID+ChatID+SenderID+
// MessageID... wait — MessageID is per-turn — but more importantly, two runs
// from different accounts must never cross-project. When Account/ChannelAppID/
// BotID are present on BOTH sides and differ, the event must NOT match. Without
// this check, account B's progress would be delivered to account A's chat.
func TestScopeMatcherRejectsDifferentAccount(t *testing.T) {
	in := channel.NormalizeInboundContext(channel.InboundContext{
		Channel:      "ilink",
		EntrypointID: "ep-1",
		ChatID:       "chat-1",
		SenderID:     "user-1",
		MessageID:    "msg-1",
		Account:      "acct-a",
		ChannelAppID: "app-a",
		BotID:        "bot-a",
	})
	m := newScopeMatcher(in)
	other := scopeFixture()
	other.Account = "acct-b" // same chat/turn, different account
	other.ChannelAppID = "app-a"
	other.BotID = "bot-a"
	if m.match(evtWithScope("agent.delegate.failed", "run-b", other)) {
		t.Fatalf("event from a different account must not match (cross-account leak)")
	}
}

// TestScopeMatcherAccountMatchWhenAligned: when Account/ChannelAppID/BotID are
// equal on both sides, the event matches (the isolation check must not reject
// same-account events).
func TestScopeMatcherAccountMatchWhenAligned(t *testing.T) {
	in := channel.NormalizeInboundContext(channel.InboundContext{
		Channel:      "ilink",
		EntrypointID: "ep-1",
		ChatID:       "chat-1",
		SenderID:     "user-1",
		MessageID:    "msg-1",
		Account:      "acct-a",
		ChannelAppID: "app-a",
		BotID:        "bot-a",
	})
	m := newScopeMatcher(in)
	sc := scopeFixture()
	sc.Account = "acct-a"
	sc.ChannelAppID = "app-a"
	sc.BotID = "bot-a"
	if !m.match(evtWithScope("agent.delegate.failed", "run-a", sc)) {
		t.Fatalf("event with matching account fields should match")
	}
}

// TestScopeMatcherSkipsEmptyAccountFields: an event carrying no Account/App/Bot
// still matches an inbound that does — not all events populate all three, so an
// empty side is skipped rather than treated as a mismatch (avoids over-rejecting
// legitimate events while still isolating when both sides are populated).
func TestScopeMatcherSkipsEmptyAccountFields(t *testing.T) {
	in := channel.NormalizeInboundContext(channel.InboundContext{
		Channel:      "ilink",
		EntrypointID: "ep-1",
		ChatID:       "chat-1",
		SenderID:     "user-1",
		MessageID:    "msg-1",
		Account:      "acct-a",
	})
	m := newScopeMatcher(in)
	sc := scopeFixture() // Account/App/Bot all empty
	if !m.match(evtWithScope("agent.delegate.failed", "run-a", sc)) {
		t.Fatalf("event with empty account fields should still match (skip, not reject)")
	}
}

// TestScopeMatcherMatchesByKnownScopeRunID: once a run id is adopted from a
// scope-matched event, a later event whose Scope.RunID is that known id matches
// even if its other scope fields differ (the run-id affiliation fast path).
func TestScopeMatcherMatchesByKnownScopeRunID(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	// First event adopts run-1 via scope match.
	if !m.match(evtWithScope("run.started", "run-1", scopeFixture())) {
		t.Fatalf("initial scope match should adopt run-1")
	}
	// A follow-up event with a different (non-matching) scope but carrying the
	// known RunID in Scope still matches.
	other := runtime.RuntimeEventScope{RunID: "run-1"} // bare scope, only RunID
	evt := runtime.RuntimeEvent{ID: "e", Kind: "tool.completed", Scope: &other}
	if !m.match(evt) {
		t.Fatalf("event with known Scope.RunID should match via affiliation")
	}
}

// TestScopeMatcherAdoptsChildRunViaCorrelation: after adopting a run id, an
// event correlated by that id as ChildRunID matches (the child-run affiliation
// path, line 65) without a scope match.
func TestScopeMatcherAdoptsChildRunViaCorrelation(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	if !m.match(evtWithScope("run.started", "parent-run", scopeFixture())) {
		t.Fatalf("initial scope match should adopt parent-run")
	}
	// An event whose Correlation.ChildRunID == parent-run matches. (In practice
	// child runs are adopted via adoptCorrelation from a parent event, but the
	// knows(ChildRunID) arm must also match when the id is already known.)
	evt := runtime.RuntimeEvent{
		ID:    "e",
		Kind:  "agent.delegate.completed",
		RunID: "child-run",
		Correlation: &runtime.RuntimeEventCorrelation{
			ParentRunID: "parent-run",
			ChildRunID:  "parent-run", // known id used as child affiliation
		},
	}
	if !m.match(evt) {
		t.Fatalf("event correlated by a known id should match")
	}
}

// TestScopeMatcherRejectsMismatchedEntrypoint: scope matching fails fast when
// EntrypointID differs (covers the first scopeMatchesInbound negative branch).
func TestScopeMatcherRejectsMismatchedEntrypoint(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	other := scopeFixture()
	other.EntrypointID = "ep-other"
	if m.match(evtWithScope("agent.delegate.failed", "run-x", other)) {
		t.Fatalf("event with a different EntrypointID must not match")
	}
}

// TestScopeMatcherRejectsMismatchedChannel: scope matching fails when Channel
// differs.
func TestScopeMatcherRejectsMismatchedChannel(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	other := scopeFixture()
	other.Channel = "feishu"
	if m.match(evtWithScope("agent.delegate.failed", "run-x", other)) {
		t.Fatalf("event with a different Channel must not match")
	}
}

// TestScopeMatcherRejectsMismatchedSender: scope matching fails when SenderID
// differs.
func TestScopeMatcherRejectsMismatchedSender(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	other := scopeFixture()
	other.SenderID = "user-other"
	if m.match(evtWithScope("agent.delegate.failed", "run-x", other)) {
		t.Fatalf("event with a different SenderID must not match")
	}
}

// TestScopeMatcherRejectsNilScope: an event with no Scope at all does not match
// (and does not panic).
func TestScopeMatcherRejectsNilScope(t *testing.T) {
	m := newScopeMatcher(inboundFixture())
	evt := runtime.RuntimeEvent{ID: "e", Kind: "agent.delegate.failed"}
	if m.match(evt) {
		t.Fatalf("event with nil Scope must not match")
	}
}

// TestScopeMatcherRejectsMismatchedChannelAppID: account isolation fails when
// ChannelAppID differs on both sides (covers that branch of accountIsolated).
func TestScopeMatcherRejectsMismatchedChannelAppID(t *testing.T) {
	in := channel.NormalizeInboundContext(channel.InboundContext{
		Channel: "ilink", EntrypointID: "ep-1", ChatID: "chat-1",
		SenderID: "user-1", MessageID: "msg-1", ChannelAppID: "app-a",
	})
	m := newScopeMatcher(in)
	sc := scopeFixture()
	sc.ChannelAppID = "app-b"
	if m.match(evtWithScope("agent.delegate.failed", "run-x", sc)) {
		t.Fatalf("event with a different ChannelAppID must not match")
	}
}

// TestScopeMatcherRejectsMismatchedBotID: account isolation fails when BotID
// differs on both sides.
func TestScopeMatcherRejectsMismatchedBotID(t *testing.T) {
	in := channel.NormalizeInboundContext(channel.InboundContext{
		Channel: "ilink", EntrypointID: "ep-1", ChatID: "chat-1",
		SenderID: "user-1", MessageID: "msg-1", BotID: "bot-a",
	})
	m := newScopeMatcher(in)
	sc := scopeFixture()
	sc.BotID = "bot-b"
	if m.match(evtWithScope("agent.delegate.failed", "run-x", sc)) {
		t.Fatalf("event with a different BotID must not match")
	}
}
