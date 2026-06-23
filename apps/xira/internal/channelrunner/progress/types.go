// Package progress implements the v0 conversation progress forwarder for IM
// channels (iLink / Feishu) that lack a runtime event feed.
//
// It subscribes to the (best-effort, per-Service global) runtime EventBus,
// keeps read/write decoupled so a slow channel SendText cannot starve event
// consumption (§8.4), matches events to the inbound request scope with
// MessageID hard isolation (§8.3), renders allowlisted runtime facts into
// short chat messages (§10), and throttles/dedupes them (§9).
//
// v0 delivers:
//   - progress (counts against MaxMessagesPerTurn): silence notice,
//     agent.delegate.failed, agent.delegate.timeout
//   - interaction signal (delivered independently of the progress quota):
//     run.waiting_human
//
// assistant.final is consumed only as a drain signal and is never rendered.
//
// See docs/architecture/xira-conversation-progress-feed-v0.zh.md.
package progress

import (
	"context"
	"time"
)

// Message is the channel-neutral progress payload handed to a Sender.
type Message struct {
	EventID string
	Kind    string
	Text    string
	Level   string
}

// Sender delivers a progress Message to a channel. Implementations must be
// safe for concurrent use by the forwarder's sender goroutine.
type Sender interface {
	SendProgress(ctx context.Context, msg Message) error
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, msg Message) error

// SendProgress implements Sender.
func (f SenderFunc) SendProgress(ctx context.Context, msg Message) error { return f(ctx, msg) }

// Policy governs throttle/dedupe/quota. MaxMessagesPerTurn constrains only
// progress messages; the waiting_human interaction signal is delivered
// independently and does not count against it.
type Policy struct {
	InitialSilenceThreshold time.Duration
	MinInterval             time.Duration
	MaxMessagesPerTurn      int
	MaxChars                int
}

// DefaultPolicy returns the v0 IM defaults (§9.1).
func DefaultPolicy() Policy {
	return Policy{
		InitialSilenceThreshold: 20 * time.Second,
		MinInterval:             12 * time.Second,
		MaxMessagesPerTurn:      2,
		MaxChars:                180,
	}
}
