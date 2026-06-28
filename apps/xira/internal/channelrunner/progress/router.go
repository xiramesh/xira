package progress

import (
	"context"
	"sync"
	"time"

	"github.com/xiramesh/xira/internal/runtime"
)

// router.go: Router is the per-chat-key turn router (Phase 4, RFC #48 §2.2).
// It replaces the serial-blocking Monitor callback:
//
//   - Message arrives → Router.Handle(chatKey, msg, ctx)
//   - No active turn for this chatKey → starts a new turn (OnNewTurn callback)
//   - Active turn running → message enqueues to SteeringQueue (steering)
//
// The Router manages a map[ChatKey]*chatEntry. Each entry has an active flag
// + a SteeringQueue. When the turn completes, the caller marks it inactive
// (TurnComplete).

// OnNewTurnFunc is called when a message should start a new turn.
// The ctx carries SteeringBus for the new turn. The caller (channel
// runner) wires up its own EventBus + Sender inside this callback.
type OnNewTurnFunc func(key runtime.ChatKey, msg string, ctx context.Context)

// Router routes incoming messages per ChatKey.
type Router struct {
	mu      sync.Mutex
	entries map[runtime.ChatKey]*chatEntry
}

type chatEntry struct {
	mu              sync.Mutex
	active          bool // true only while a turn goroutine is running (committed)
	reserved        bool // true between Route(started) and Start/Abort — pending, not yet committed
	activeRequestID string // request_id of the in-flight turn (cited by steered acks)
	steering        *SteeringQueue
	spawn           *SpawnCollector
	idleSince       time.Time // set when active flips to false; used by prune
}

// routerEntryTTL is how long an idle entry (no active turn) is kept before
// eviction. Mirrors the dedupe TTL (1h): an hour without a message means the
// conversation is almost certainly over. Eviction frees the entry's
// SteeringQueue + SpawnCollector; a later message rebuilds a fresh entry.
const routerEntryTTL = time.Hour

// NewRouter creates a Router.
func NewRouter() *Router {
	return &Router{
		entries: make(map[runtime.ChatKey]*chatEntry),
	}
}

// Handle routes a message AND starts the turn immediately (Route + Start). Use
// this from channels whose pre-turn registration is already complete when they
// route (ilink/feishu: ack/dedupe handled by the SDK before routing). Websocket
// must NOT use this — it needs addActive + accepted ack between Route and Start
// (PR #97 round-5 review); it uses ChatKeySession.Route instead.
//
// requestID is recorded on the entry so a later steered message's outcome can
// cite it as the reply target. Returns true if steered, false if started.
func (r *Router) Handle(key runtime.ChatKey, requestID, msg string, parentCtx context.Context, onNewTurn OnNewTurnFunc) bool {
	outcome := r.Route(key, requestID, msg, parentCtx, onNewTurn)
	if outcome.Steered {
		return true
	}
	outcome.Start()
	return false
}

// RoutingOutcome is the atomic routing decision for one message (PR #97
// round-5 review). Route decides under entry.mu whether a message STARTS a
// turn or is STEERED, but for a started turn it does NOT launch the goroutine
// — the caller calls Start() after completing pre-turn registration (websocket
// needs addActive + accepted ack before any frame can be produced).
//
// Steered: the message was enqueued into the active turn's SteeringQueue. It
// 	will not run its own turn; its reply comes via the active turn's OnTurnResult.
// 	ActiveRequestID is the active turn's request_id (cited in the steered ack
// 	so the client knows which request_id the eventual terminal carries — scheme P).
// Started: the message began a new turn. The caller MUST call Start() (or
// 	Abort() if it decides not to proceed, e.g. ack write failed).
type RoutingOutcome struct {
	Steered         bool
	ActiveRequestID string // valid when Steered
	start           func()
	abort           func()
}

// Start launches the turn goroutine. Idempotent (no-op if already started,
// aborted, or steered). The caller MUST ensure pre-turn registration is
// complete first.
func (o RoutingOutcome) Start() {
	if o.start != nil {
		o.start()
	}
}

// Abort releases the Router entry without running the turn — for when the
// caller routed "started" but then decided not to proceed (e.g. the accepted
// ack write failed because the connection dropped). Idempotent. No-op if
// steered or already started.
func (o RoutingOutcome) Abort() {
	if o.abort != nil {
		o.abort()
	}
}

// Route routes a message and returns the atomic outcome WITHOUT starting the
// turn goroutine (for started turns). requestID is recorded on the entry so a
// later steered message's outcome.ActiveRequestID can cite it. The caller calls
// outcome.Start() once pre-turn registration is done. Handle wraps Route+Start
// for channels that don't need the gap (ilink/feishu).
//
// A started turn is RESERVED (entry.reserved=true), NOT active. Only Start()
// flips it to active. This means a second message arriving during the
// pending-start window (after Route, before Start) sees active=false and STARTS
// its own turn — it is NOT steered into the uncommitted one. This prevents the
// orphan bug where a steered message cites a request_id whose turn later Aborts
// (PR #97 round-6 review CRITICAL #1).
func (r *Router) Route(key runtime.ChatKey, requestID, msg string, parentCtx context.Context, onNewTurn OnNewTurnFunc) RoutingOutcome {
	// Lazy prune: evict entries idle longer than the TTL before routing, so
	// Router.entries doesn't grow unboundedly (mirrors dedupe.pruneLocked).
	r.prune(time.Now())

	entry := r.getOrCreate(key)
	entry.mu.Lock()
	if entry.active {
		activeID := entry.activeRequestID
		entry.steering.Enqueue(msg)
		entry.mu.Unlock()
		return RoutingOutcome{Steered: true, ActiveRequestID: activeID}
	}
	// Reserve the entry (NOT active). Until Start() commits it to active, a
	// concurrent message sees active=false and starts its own turn — it cannot
	// be orphaned into this uncommitted one.
	entry.reserved = true
	entry.activeRequestID = requestID
	sq := entry.steering
	entry.mu.Unlock()

	// Wire SteeringBus + SpawnBus into context for the new turn. Both are
	// per-chat-key sinks: SteeringBus carries user interjections (steering
	// checkpoint), SpawnBus carries spawned child results (wait_turn). Every
	// channel that uses the Router gets both for free.
	ctx := runtime.WithSteeringBus(parentCtx, sq)
	ctx = runtime.WithSpawnBus(ctx, entry.spawn)

	started := false
	return RoutingOutcome{
		start: func() {
			if started {
				return
			}
			started = true
			entry.mu.Lock()
			entry.reserved = false
			entry.active = true
			entry.mu.Unlock()
			go func() {
				defer r.markComplete(key)
				onNewTurn(key, msg, ctx)
			}()
		},
		abort: func() {
			if started {
				return
			}
			started = true
			entry.mu.Lock()
			entry.reserved = false
			entry.activeRequestID = ""
			entry.mu.Unlock()
		},
	}
}

// markComplete marks the turn as inactive for this chatKey.
func (r *Router) markComplete(key runtime.ChatKey) {
	r.mu.Lock()
	entry, ok := r.entries[key]
	r.mu.Unlock()
	if !ok {
		return
	}
	entry.mu.Lock()
	entry.active = false
	entry.reserved = false
	entry.activeRequestID = ""
	entry.idleSince = time.Now()
	entry.mu.Unlock()
}

// IsActive reports whether a turn is currently in flight for chatKey. Exported
// so cross-package tests (feishu/ws) can wait for a dispatched turn to finish
// before asserting on its side-effects (which run async in the router
// goroutine). Lock order: r.mu → entry.mu (consistent with Handle/markComplete).
func (r *Router) IsActive(key runtime.ChatKey) bool {
	r.mu.Lock()
	entry, ok := r.entries[key]
	r.mu.Unlock()
	if !ok {
		return false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.active
}

// ActiveRequestID returns the request_id of the in-flight (committed active)
// turn for chatKey, or "" if none. Used by websocket's duplicate-ack path to
// tell a reconnecting client which request_id the active turn's terminal will
// carry (scheme P). Lock order: r.mu → entry.mu.
func (r *Router) ActiveRequestID(key runtime.ChatKey) string {
	r.mu.Lock()
	entry, ok := r.entries[key]
	r.mu.Unlock()
	if !ok {
		return ""
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.active {
		return ""
	}
	return entry.activeRequestID
}

// prune evicts entries that are idle (no active turn) and have been idle
// longer than routerEntryTTL. Called lazily from Handle (on-access, mirroring
// dedupe.pruneLocked) — no background goroutine. Safe to call with any "now"
// (tests pass a future time to simulate TTL elapsing).
//
// Lock order: r.mu → entry.mu (consistent with Handle/markComplete).
func (r *Router) prune(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, entry := range r.entries {
		entry.mu.Lock()
		active := entry.active
		idleSince := entry.idleSince
		entry.mu.Unlock()
		// Never evict an active turn. Evict idle entries past their TTL.
		if active {
			continue
		}
		if idleSince.IsZero() {
			// idleSince zero = entry created but never ran a turn (shouldn't
			// normally happen; treat as idle since creation — skip, it's
			// about to be used by the Handle that called prune).
			continue
		}
		if now.Sub(idleSince) > routerEntryTTL {
			delete(r.entries, key)
		}
	}
}

// warpIdleSince forces an entry's idleSince (test-only: simulates TTL
// elapsing without sleeping). No-op if the entry doesn't exist.
func (r *Router) warpIdleSince(key runtime.ChatKey, idleSince time.Time) {
	r.mu.Lock()
	entry, ok := r.entries[key]
	r.mu.Unlock()
	if !ok {
		return
	}
	entry.mu.Lock()
	entry.idleSince = idleSince
	entry.mu.Unlock()
}

// SteeringQueue returns the SteeringQueue for a chatKey (for testing/inspection).
func (r *Router) SteeringQueue(key runtime.ChatKey) *SteeringQueue {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[key]
	if !ok {
		return nil
	}
	return entry.steering
}

// SpawnCollectorFor returns the SpawnCollector for a chatKey. The channel
// runner uses it on steering retry to Reset stale spawn results (mirrors
// ChatContext.Reset). Returns nil if the chatKey has no entry.
func (r *Router) SpawnCollectorFor(key runtime.ChatKey) *SpawnCollector {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[key]
	if !ok {
		return nil
	}
	return entry.spawn
}

// getOrCreate returns the chatEntry for key, creating if absent.
func (r *Router) getOrCreate(key runtime.ChatKey) *chatEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[key]
	if !ok {
		entry = &chatEntry{
			steering: NewSteeringQueue(),
			spawn:    NewSpawnCollector(),
		}
		r.entries[key] = entry
	}
	return entry
}
