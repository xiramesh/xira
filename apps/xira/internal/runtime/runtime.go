package runtime

import (
	"context"
	"errors"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
)

var ErrHumanRequestDeliveryUnsupported = errors.New("human request delivery unsupported")

// runtime.go: package-level interface declarations for the runtime package.
//
// Interfaces live here (not on service.go) so they're easy to find and don't
// bloat the 600+-line Service file. Each interface is satisfied implicitly by
// *Service; the var _ assertions below guard against silent breakage if
// Service's method signatures drift.

// Runtime is the injectable subset of *Service that callers outside the
// runtime package depend on. Declared as an interface so unit tests in
// other packages (e.g. channelrunner/progress.ChatKeySession) can fake
// RunAgent without constructing a real Service with stores/LLM/agents.
//
// *Service satisfies this implicitly (service.go RunAgent).
type Runtime interface {
	RunAgent(ctx context.Context, req TurnRequest) (TurnResponse, error)
}

// ExactHITLResolver is the channel-neutral structured response surface used by
// native buttons and explicit request-id protocols.
type ExactHITLResolver interface {
	ResolveHumanResponse(ctx context.Context, input humanrequest.HumanResponseEnvelope) (*humanrequest.HumanRequest, error)
}

// AsyncExactHITLResolver atomically accepts a native response, then schedules
// the existing durable Agent/Flow resume without making a platform callback
// wait for model work.
type AsyncExactHITLResolver interface {
	ResolveHumanResponseAsync(ctx context.Context, input humanrequest.HumanResponseEnvelope) (*humanrequest.HumanRequest, error)
}

// StructuredHITLResolver is the channel-neutral surface for transports whose
// response frame carries only opaque request correlation. The runner first
// loads the persisted request to bind transport authority, then commits and
// resumes through the shared async state machine.
type StructuredHITLResolver interface {
	GetHumanRequest(ctx context.Context, requestID string) (*humanrequest.HumanRequest, error)
	ResolveHumanResponseAsync(ctx context.Context, input humanrequest.HumanResponseEnvelope) (*humanrequest.HumanRequest, error)
}

// TextHITLResolver resolves a parsed explicit text reference with authoritative
// runner identity. It is separate from legacy same-chat HITL resolution.
type TextHITLResolver interface {
	ResolveHumanTextResponse(ctx context.Context, input humanrequest.TextResponseEnvelope) (*humanrequest.HumanRequest, error)
}

type HumanRequestDeliveryTarget struct {
	Route     channel.InboundContext
	Recipient *channel.OutboundRecipient
}

type HumanRequestDeliveryReceipt struct {
	MessageID string
}

// HumanRequestDeliverer is the receipt-returning adapter port for interactive
// request presentation. Validate must be route-local and side-effect free.
type HumanRequestDeliverer interface {
	ValidateHumanRequestDelivery(HumanRequestDeliveryTarget) error
	DeliverHumanRequest(context.Context, humanrequest.HumanRequest, HumanRequestDeliveryTarget) (HumanRequestDeliveryReceipt, error)
}

// OwnerResolver is the injectable subset of *Service for owner queries (#122).
// Channel runners use it to let the owner bypass the sender allowlist (#121)
// even when they aren't explicitly listed. nil = owner concept not configured
// (#121 only: allowlist-only auth, owner bypass disabled).
//
// IsOwner takes entrypointID (not channel) because a channel can host multiple
// entrypoints (e.g. feishu-expense-bot + feishu-leave-bot both on "feishu"),
// each with its own owner. The earlier draft (#134) used channel, but that
// would let one entrypoint's owner bypass another entrypoint's allowlist —
// a cross-entrypoint privilege escalation. entrypointID scopes the lookup
// to exactly the entrypoint the runner is handling.
type OwnerResolver interface {
	IsOwner(ctx context.Context, senderID, entrypointID string) bool
}

// OwnerTargetResolver is deliberately separate from OwnerResolver: a boolean
// authorization answer is not a routable private-delivery address.
type OwnerTargetResolver interface {
	ResolveOwnerTarget(ctx context.Context, entrypointID string) (OwnerDeliveryTarget, error)
}

// Compile-time assertions for the runtime surfaces injected into adapters.
var _ Runtime = (*Service)(nil)
var _ ExactHITLResolver = (*Service)(nil)
var _ AsyncExactHITLResolver = (*Service)(nil)
var _ StructuredHITLResolver = (*Service)(nil)
var _ TextHITLResolver = (*Service)(nil)
var _ OwnerResolver = (*Service)(nil)
var _ OwnerTargetResolver = (*Service)(nil)
