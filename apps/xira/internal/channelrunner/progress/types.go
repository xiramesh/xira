// Package progress implements per-chat-key conversation progress delivery for
// channels (iLink / Feishu / websocket) that need chat-facing progress.
//
// There is no per-Service global EventBus and no Forwarder subscription path.
// Runtime signal events are delivered point-to-point through context-carried
// sinks: runtime.EventBus for sealed Event rendering, or runtime.RawEventSink
// when a channel renders flat RuntimeEvents itself. ChatContext keeps
// read/write decoupled, renders allowlisted runtime facts into short chat
// messages, and applies quota/dedupe.
//
// v0 delivers:
//   - progress (counts against parent/child progress quotas): silence notice,
//     agent.delegate.failed, agent.delegate.timeout
//   - interaction signal (delivered independently of the progress quota):
//     run.waiting_human
//
// assistant.final is consumed only as a drain signal and is never rendered.
//
// See docs/architecture/xira-runtime-current-contract.zh.md.
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
// safe for concurrent use by ChatContext's sender goroutine.
type Sender interface {
	SendProgress(ctx context.Context, msg Message) error
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, msg Message) error

// SendProgress implements Sender.
func (f SenderFunc) SendProgress(ctx context.Context, msg Message) error { return f(ctx, msg) }

// Policy governs throttle/dedupe/quota. Progress quotas constrain only agent
// progress messages; the waiting_human interaction signal is delivered
// independently and does not count against them.
type Policy struct {
	InitialSilenceThreshold time.Duration
	MinInterval             time.Duration
	// MaxMessagesPerTurn is the legacy shared progress cap. It is retained as
	// fallback when the parent/child-specific caps below are unset.
	MaxMessagesPerTurn               int
	MaxParentProgressMessagesPerTurn int
	MaxChildProgressMessagesPerTurn  int
	MaxChars                         int
}

// DefaultPolicy returns the v0 IM defaults (§9.1).
func DefaultPolicy() Policy {
	return Policy{
		InitialSilenceThreshold:          20 * time.Second,
		MinInterval:                      12 * time.Second,
		MaxMessagesPerTurn:               3,
		MaxParentProgressMessagesPerTurn: 3,
		MaxChildProgressMessagesPerTurn:  2,
		MaxChars:                         180,
	}
}

func progressQuotaLimit(policy Policy, child bool) int {
	if child {
		if policy.MaxChildProgressMessagesPerTurn > 0 {
			return policy.MaxChildProgressMessagesPerTurn
		}
		return policy.MaxMessagesPerTurn
	}
	if policy.MaxParentProgressMessagesPerTurn > 0 {
		return policy.MaxParentProgressMessagesPerTurn
	}
	return policy.MaxMessagesPerTurn
}
