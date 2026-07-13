package runtime

import (
	"context"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
)

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

// HITLResolver is the injectable subset of *Service for HITL resolve/query.
// Channel adapters use it to check "does this chatKey have a pending HITL?"
// and to resolve it from an IM reply (#92 — HITL IM direct answer). Separate
// from Runtime so fakes for RunAgent-only tests don't need to implement HITL
// methods; channel adapters that want HITL direct-answer set this field (nil =
// HITL direct-answer disabled, messages always start a new turn).
//
// *Service satisfies this implicitly.
type HITLResolver interface {
	ListPendingHumanRequestsByChatKey(ctx context.Context, chatKey string) ([]humanrequest.HumanRequest, error)
	ResolveHumanRequest(ctx context.Context, requestID string, input humanrequest.ResolveRequest) (*humanrequest.HumanRequest, error)
}

// ExactHITLResolver is the channel-neutral structured response surface used by
// native buttons and explicit request-id protocols.
type ExactHITLResolver interface {
	ResolveHumanResponse(ctx context.Context, input humanrequest.HumanResponseEnvelope) (*humanrequest.HumanRequest, error)
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

// Compile-time assertions: *Service implements Runtime + HITLResolver + OwnerResolver.
var _ Runtime = (*Service)(nil)
var _ HITLResolver = (*Service)(nil)
var _ ExactHITLResolver = (*Service)(nil)
var _ TextHITLResolver = (*Service)(nil)
var _ OwnerResolver = (*Service)(nil)
var _ OwnerTargetResolver = (*Service)(nil)
