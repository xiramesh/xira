package runtime

import "context"

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

// Compile-time assertion: *Service implements Runtime. If a future change
// to Service.RunAgent's signature breaks this, the build fails here rather
// than at every call site.
var _ Runtime = (*Service)(nil)
