package progress

import "sync"

// steering_queue.go: SteeringQueue is the per-chat-key steering queue
// (Phase 4, RFC #48 §5). When a user sends a message while a turn is
// running, it goes here instead of starting a new turn.
//
// The ADK event loop (generateADK) checks this queue between iterations.
// If non-empty, the current run is canceled and restarted with the
// user's interjection as a new message.
//
// Implements runtime.SteeringBus.

// SteeringQueue is a thread-safe FIFO queue of user interjections.
type SteeringQueue struct {
	mu   sync.Mutex
	msgs []string
}

// NewSteeringQueue creates an empty SteeringQueue.
func NewSteeringQueue() *SteeringQueue {
	return &SteeringQueue{
		msgs: make([]string, 0, 4),
	}
}

// Enqueue adds a user interjection to the queue.
func (sq *SteeringQueue) Enqueue(message string) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	sq.msgs = append(sq.msgs, message)
}

// TryDequeue removes and returns the oldest interjection.
// Returns ok=false if the queue is empty.
func (sq *SteeringQueue) TryDequeue() (string, bool) {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	if len(sq.msgs) == 0 {
		return "", false
	}
	msg := sq.msgs[0]
	sq.msgs = sq.msgs[1:]
	return msg, true
}

// DrainAll removes and returns all pending interjections.
func (sq *SteeringQueue) DrainAll() []string {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	out := make([]string, len(sq.msgs))
	copy(out, sq.msgs)
	sq.msgs = sq.msgs[:0]
	return out
}

// HasPending reports whether the queue has any interjections, WITHOUT
// consuming them. Used by the steering checkpoint (peek only — the retry
// loop consumes via TryDequeue).
func (sq *SteeringQueue) HasPending() bool {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return len(sq.msgs) > 0
}
