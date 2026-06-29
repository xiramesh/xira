package runtime

import (
	"context"

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

// Compile-time assertions: *Service implements Runtime + HITLResolver.
var _ Runtime = (*Service)(nil)
var _ HITLResolver = (*Service)(nil)
