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
