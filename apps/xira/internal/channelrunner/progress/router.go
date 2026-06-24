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
// The ctx carries SteeringSink for the new turn. The caller (channel
// runner) wires up its own EventSink + Sender inside this callback.
type OnNewTurnFunc func(key runtime.ChatKey, msg string, ctx context.Context)

// Router routes incoming messages per ChatKey.
type Router struct {
	mu      sync.Mutex
	entries map[runtime.ChatKey]*chatEntry
}

type chatEntry struct {
	mu        sync.Mutex
	active    bool
	steering  *SteeringQueue
	spawn     *SpawnCollector
	idleSince time.Time // set when active flips to false; used by prune
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

// Handle routes a message: starts a new turn (calls onNewTurn) or steers
// an active one (enqueues to SteeringQueue). The onNewTurn callback is
// per-call — the caller passes its own closure with per-message context
// (account, sender, etc.) that Router's lifecycle can't know about.
func (r *Router) Handle(key runtime.ChatKey, msg string, parentCtx context.Context, onNewTurn OnNewTurnFunc) {
	// Lazy prune: evict entries idle longer than the TTL before routing, so
	// Router.entries doesn't grow unboundedly (mirrors dedupe.pruneLocked).
	r.prune(time.Now())

	entry := r.getOrCreate(key)
	entry.mu.Lock()
	if entry.active {
		entry.steering.Enqueue(msg)
		entry.mu.Unlock()
		return
	}
	entry.active = true
	sq := entry.steering
	entry.mu.Unlock()

	// Wire SteeringSink + SpawnSink into context for the new turn. Both are
	// per-chat-key sinks: SteeringSink carries user interjections (steering
	// checkpoint), SpawnSink carries spawned child results (wait_turn). Every
	// channel that uses the Router gets both for free.
	ctx := runtime.WithSteeringSink(parentCtx, sq)
	ctx = runtime.WithSpawnSink(ctx, entry.spawn)

	// Run the turn in a goroutine so Handle returns immediately
	// (non-blocking — Monitor can receive the next message).
	go func() {
		defer r.markComplete(key)
		onNewTurn(key, msg, ctx)
	}()
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
	entry.idleSince = time.Now()
	entry.mu.Unlock()
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
