package progress

import (
	"sync"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/runtime"
)

// scopeMatcher decides whether a runtime event belongs to the inbound request
// this forwarder is bound to.
//
// The EventBus is a per-Service global singleton. Two consecutive direct-chat
// turns share EntrypointID+ChatID+SenderID, so MessageID is the only turn
// separator before the run id is known. Matching therefore:
//  1. Refuses to match anything when the inbound request has no MessageID
//     (no safe isolation exists -> stay silent, §8.3).
//  2. Once a run id is discovered, matches events affiliated by run id,
//     trace id, parent/child run id.
//  3. Otherwise matches the full inbound scope INCLUDING MessageID.
//
// See docs/architecture/xira-conversation-progress-feed-v0.zh.md §8.3.
type scopeMatcher struct {
	inbound          channel.InboundContext
	requireMessageID bool

	mu     sync.Mutex
	runIDs map[string]struct{}
}

func newScopeMatcher(inbound channel.InboundContext) *scopeMatcher {
	return &scopeMatcher{
		inbound:          inbound,
		requireMessageID: inbound.MessageID != "",
		runIDs:           make(map[string]struct{}),
	}
}

func (m *scopeMatcher) match(evt runtime.RuntimeEvent) bool {
	if !m.requireMessageID {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Already-known run affiliation.
	if evt.RunID != "" && m.knows(evt.RunID) {
		m.adoptCorrelation(evt)
		return true
	}
	if evt.Scope != nil && evt.Scope.RunID != "" && m.knows(evt.Scope.RunID) {
		m.adoptCorrelation(evt)
		return true
	}
	if evt.Correlation != nil {
		if id := evt.Correlation.TraceID; id != "" && m.knows(id) {
			m.adoptCorrelation(evt)
			return true
		}
		if id := evt.Correlation.ParentRunID; id != "" && m.knows(id) {
			m.adoptCorrelation(evt)
			return true
		}
		if id := evt.Correlation.ChildRunID; id != "" && m.knows(id) {
			return true
		}
	}

	// Fall back to inbound scope match (with MessageID hard isolation).
	if m.scopeMatchesInbound(evt) {
		m.adoptRunIDs(evt)
		return true
	}
	return false
}

func (m *scopeMatcher) knows(id string) bool {
	_, ok := m.runIDs[id]
	return ok
}

// adoptCorrelation records a child run id discovered via correlation so future
// events from that child match directly.
func (m *scopeMatcher) adoptCorrelation(evt runtime.RuntimeEvent) {
	if evt.Correlation != nil {
		if id := evt.Correlation.ChildRunID; id != "" {
			m.runIDs[id] = struct{}{}
		}
	}
}

// adoptRunIDs registers the run id(s) carried by a scope-matched event so
// follow-up events (which may drop the scope) still match.
func (m *scopeMatcher) adoptRunIDs(evt runtime.RuntimeEvent) {
	if evt.RunID != "" {
		m.runIDs[evt.RunID] = struct{}{}
	}
	if evt.Scope != nil && evt.Scope.RunID != "" {
		m.runIDs[evt.Scope.RunID] = struct{}{}
	}
}

func (m *scopeMatcher) scopeMatchesInbound(evt runtime.RuntimeEvent) bool {
	sc := evt.Scope
	if sc == nil {
		return false
	}
	if sc.EntrypointID != m.inbound.EntrypointID {
		return false
	}
	if sc.Channel != m.inbound.Channel {
		return false
	}
	if sc.ChatID != m.inbound.ChatID {
		return false
	}
	if sc.SenderID != m.inbound.SenderID {
		return false
	}
	// MessageID hard isolation: the mandatory turn separator.
	if sc.MessageID != m.inbound.MessageID {
		return false
	}
	return true
}
