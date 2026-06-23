package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// spawn_turn.go: async child turn spawn (Phase 3, RFC §2.4 corrected).
//
// spawn_turn replaces delegate_agent's synchronous "block until child
// finishes" model with "return immediately, child runs in detached
// goroutine". The parent LLM sees {"agent_turn_id":"...", "status":"spawned"}
// as the tool result and continues reasoning.
//
// D-3 (RFC): spawn result payload goes via pendingResults channel (not
// EventBus). EventBus only gets AgentTurnCompleted signal (no payload).
//
// The ADK StreamingFunctionTool wrapper (registered in runtimeADKTools)
// is thin: it calls spawnCore, yields the spawned status, then closes
// the iterator. The real logic is in spawnCore.

// spawnTurnToolName is the ADK tool name.
const spawnTurnToolName = "spawn_turn"

// spawnSpec is the validated input for a spawn operation.
type spawnSpec struct {
	AgentID string
	Task    string
}

// Validate checks required fields.
func (s spawnSpec) Validate() error {
	if s.AgentID == "" {
		return fmt.Errorf("agent_id is required")
	}
	if s.Task == "" {
		return fmt.Errorf("task is required")
	}
	return nil
}

// spawnResult is the immediate return of spawnCore — what the parent LLM
// sees as the tool result.
type spawnResult struct {
	TurnID string
	Status string // always "spawned"
}

// pendingResult is what the detached goroutine delivers when the child
// turn finishes. Consumed by the parent's retry/checkpoint loop (Phase 4
// steering checkpoint + future wait_turn tool).
type pendingResult struct {
	TurnID string
	Result DelegateAgentResult
	Err    string
}

// spawnTarget abstracts the child turn executor. In production, it's
// a closure calling s.RunChildAgent. In tests, it's a mock.
type spawnTarget interface {
	Run(ctx context.Context, agentID, task string) (DelegateAgentResult, error)
}

// spawnCore is the core spawn logic: validate, generate turn ID, detach
// goroutine, return immediately.
//
// The detached goroutine uses context.WithoutCancel(parentCtx) so the
// child survives parent tool-return (ADK cancels the tool ctx after the
// iterator closes). The child result goes to SpawnSink (looked up from
// parentCtx via SpawnSinkFromContext). When no sink is present, the result
// is dropped with a Warn log (Phase 3 fire-and-forget — sink consumers
// arrive in Phase 4/5).
//
// signalBus is optional (nil = no signal published). When present,
// AgentTurnCompleted signal is published on child completion.
func spawnCore(parentCtx context.Context, spec spawnSpec, target spawnTarget, signalBus EventBus) spawnResult {
	turnID := newSpawnTurnID()
	sink := SpawnSinkFromContext(parentCtx)

	// Detached context: child survives parent ctx cancel (ADK cancels tool
	// ctx after iterator closes). WithoutCancel is Go 1.21+.
	childCtx := context.WithoutCancel(parentCtx)

	go func() {
		result, err := target.Run(childCtx, spec.AgentID, spec.Task)

		pr := pendingResult{TurnID: turnID}
		if err != nil {
			pr.Err = errString(err)
			slog.Warn("spawn_turn child failed",
				"turn_id", turnID,
				"agent_id", spec.AgentID,
				"error", pr.Err)
		} else {
			pr.Result = result
			slog.Info("spawn_turn child completed",
				"turn_id", turnID,
				"agent_id", spec.AgentID,
				"status", result.Status)
		}

		// D-3: deliver result payload via SpawnSink. Sink is non-blocking
		// (Deliver contract); nil sink = drop + Warn (Phase 3 has no
		// production sink yet — fire-and-forget).
		if sink != nil {
			sink.Deliver(pr)
		} else {
			slog.Warn("spawn_turn child result dropped (no SpawnSink in context)",
				"turn_id", turnID,
				"agent_id", spec.AgentID)
		}

		// D-3: signal on EventBus (no payload — just "child completed").
		if signalBus != nil {
			signalBus.PublishEvent(AgentTurnCompleted{
				MessageIDVal:   "spawn_complete_" + turnID,
				AgentTurnIDVal: AgentTurnID(turnID),
				TimestampVal:   time.Now(),
				TurnKind:       AgentTurnKindAgent,
			})
		}
	}()

	return spawnResult{TurnID: turnID, Status: "spawned"}
}

// newSpawnTurnID generates a unique turn ID for a spawned child.
func newSpawnTurnID() string {
	return "spawn:" + uuid.NewString()[:8]
}
