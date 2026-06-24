package progress

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/xiramesh/xira/internal/runtime"
)

// child_cancel_registry.go: ChildCancelRegistry tracks spawned-child cancel
// funcs by chatKey, so that when the parent turn is steered (RFC #67), the
// channel runner can cancel every outstanding child of that conversation —
// stopping them from burning tokens after the user said "算了别做了".
//
// Lifecycle mirrors SpawnCollector / SteeringQueue (per-chat-key, RFC #48):
// one registry lives for the turn (created in runTurn alongside chatCtx),
// is injected into the run ctx, and Reset(chatKey) is called when the turn
// ends to prevent leaks.
//
// Why chatKey (not parentRunID): steer retry calls RunAgent again, which
// generates a NEW run id each time (service.go NewRunID). A run-id-keyed
// registry would lose the prior run's children on retry. chatKey is stable
// across the conversation, matching SpawnCollector's dimension.
//
// Cancellation result handling (RFC §5): when a child is canceled, its
// detached goroutine's defer chain still runs — it delivers a PendingResult
// with Err="context canceled" to the SpawnBus. By then the SpawnCollector
// has been Reset() (runner.go:689 on steer), so the result lands in an empty
// map and is read by the NEXT turn. This is the existing, documented behavior
// ("late Deliver after Reset is harmless", runner.go:649-651) — no new logic
// needed. Adding PendingResult.Status="canceled" is #68's scope, not here.
//
// Identity via token: each registered cancel is paired with a unique uint64
// token. Unregister matches by token (not by comparing func values, which Go
// forbids for arbitrary closures). The token is returned via the unregister
// handle's closure, keeping the API simple.

var cancelTokenSeq uint64

// cancelEntry pairs a cancel func with its unique identity token.
type cancelEntry struct {
	token  uint64
	cancel context.CancelFunc
}

// ChildCancelRegistry tracks spawned-child cancel funcs per chatKey.
// Thread-safe.
type ChildCancelRegistry struct {
	mu      sync.Mutex
	cancels map[runtime.ChatKey][]cancelEntry
}

// NewChildCancelRegistry creates an empty registry.
func NewChildCancelRegistry() *ChildCancelRegistry {
	return &ChildCancelRegistry{
		cancels: make(map[runtime.ChatKey][]cancelEntry),
	}
}

// Register adds a child cancel func under the given chatKey. It returns an
// unregister handle that the caller MUST invoke when the child goroutine
// exits (normally or via cancel), so the slice does not grow unboundedly
// across many children. The cancel func itself is idempotent (safe to invoke
// again after unregister, e.g. by a late CancelAll).
func (r *ChildCancelRegistry) Register(key runtime.ChatKey, cancel context.CancelFunc) (unregister func()) {
	token := atomic.AddUint64(&cancelTokenSeq, 1)
	r.mu.Lock()
	r.cancels[key] = append(r.cancels[key], cancelEntry{token: token, cancel: cancel})
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { r.remove(key, token) })
	}
}

// remove deletes the entry with the given token from the key's slice.
func (r *ChildCancelRegistry) remove(key runtime.ChatKey, token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.cancels[key]
	for i, e := range s {
		if e.token == token {
			r.cancels[key] = append(s[:i], s[i+1:]...)
			break
		}
	}
	if len(r.cancels[key]) == 0 {
		delete(r.cancels, key)
	}
}

// CancelAll invokes every registered cancel func for the chatKey and returns
// how many were outstanding. Idempotent: calling again still reports the same
// count (funcs remain registered until unregistered by the child's defer),
// and invoking a cancel func more than once is safe.
func (r *ChildCancelRegistry) CancelAll(key runtime.ChatKey) int {
	r.mu.Lock()
	funcs := r.cancels[key]
	cp := make([]context.CancelFunc, len(funcs))
	for i, e := range funcs {
		cp[i] = e.cancel
	}
	r.mu.Unlock()
	for _, c := range cp {
		c()
	}
	return len(cp)
}

// Reset clears all cancel funcs for a chatKey (turn-end cleanup). Does NOT
// invoke them — the children's own goroutines handle their exit. Used by the
// runner's turn-end defer to prevent the registry from retaining stale
// entries across turns.
func (r *ChildCancelRegistry) Reset(key runtime.ChatKey) {
	r.mu.Lock()
	delete(r.cancels, key)
	r.mu.Unlock()
}
