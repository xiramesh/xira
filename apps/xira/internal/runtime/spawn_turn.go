package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"

	"github.com/xiramesh/xira/internal/agents"
)

// spawn_turn.go: async child turn spawn (Phase 3, RFC §2.4 corrected).
//
// spawn_turn replaces delegate_agent's synchronous "block until child
// finishes" model with "return immediately, child runs in detached
// goroutine". The parent LLM sees {"agent_turn_id":"...", "status":"spawned"}
// as the tool result and continues reasoning.
//
// D-3 (RFC): spawn result payload goes via SpawnSink (not EventBus).
// EventBus only gets AgentTurnCompleted signal (no payload).
//
// The ADK tool wrapper (registered in runtimeADKTools via functiontool.New)
// is thin: it builds a spawnSpec + serviceSpawnTarget, calls spawnCore, and
// returns {agent_turn_id, status} as the FunctionResponse map. The real
// logic is in spawnCore.

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

// spawnTurnInputSchema is the ADK input schema for spawn_turn. Phase 3 only
// accepts agent_id + task (the same minimal set delegate_agent started with).
// context_refs / max_duration_ms arrive in later phases.
func spawnTurnInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"agent_id": {Type: "string"},
			"task":     {Type: "string"},
		},
		Required:             []string{"agent_id", "task"},
		AdditionalProperties: rejectAllSchema(),
	}
}

// sanitizeSpawnTurnInput extracts {agent_id, task} from raw args and reports
// unsupported fields for transparency (mirrors sanitizeDelegateInput shape).
func sanitizeSpawnTurnInput(args map[string]any) (spawnSpec, map[string]any, []string) {
	clean := map[string]any{}
	spec := spawnSpec{}
	var unsupported []string
	for k, v := range args {
		switch k {
		case "agent_id":
			s := strings.TrimSpace(fmt.Sprint(v))
			spec.AgentID = s
			clean["agent_id"] = s
		case "task":
			s := fmt.Sprint(v)
			spec.Task = s
			clean["task"] = s
		default:
			unsupported = append(unsupported, k)
		}
	}
	return spec, clean, unsupported
}

// serviceSpawnTarget is the production spawnTarget: it runs a child turn via
// s.RunChildAgent. Phase 3 does minimal policy validation (agent exists +
// delegation policy allows the target); depth / outstanding accounting
// arrive in Phase 4/5 when SpawnSink consumers do.
type serviceSpawnTarget struct {
	service     *Service
	caller      agents.Profile
	parentBase  runtimeEventBase
	parentRunID string
	toolCallID  string
	parentDepth int
	sessionMode string
}

// Run executes the child turn synchronously (called inside spawnCore's
// detached goroutine, so it does NOT block the parent LLM).
func (t *serviceSpawnTarget) Run(ctx context.Context, agentID, task string) (DelegateAgentResult, error) {
	target, exists := t.service.agents.Get(agentID)
	if !exists {
		return DelegateAgentResult{AgentID: agentID, Status: "failed"}, fmt.Errorf("target agent %q is not registered", agentID)
	}
	policy := t.caller.NormalizedDelegationPolicy()
	if !policy.Enabled || !policy.Allows(target.ID) {
		return DelegateAgentResult{AgentID: agentID, Status: "rejected"}, fmt.Errorf("caller %q is not allowed to spawn %q", t.caller.ID, agentID)
	}
	childRunID := NewRunID(target.ID, time.Now()) + "-" + shortID()
	req := childAgentRequest{
		ParentBase:  t.parentBase,
		ParentRunID: t.parentRunID,
		ChildRunID:  childRunID,
		ToolCallID:  t.toolCallID,
		Target:      target,
		Message:     task,
		SessionMode: t.sessionMode,
		Depth:       t.parentDepth + 1,
	}
	resp, err := t.service.RunChildAgent(ctx, req)
	if err != nil {
		return DelegateAgentResult{AgentID: agentID, RunID: childRunID, Status: "failed"}, err
	}
	// Phase 3: surface Status + raw FinalResponse as Summary. Structured
	// parsing (evidence/limitations/confidence) is the SpawnSink consumer's
	// job (Phase 4/5), not spawn's — spawn is fire-and-forget.
	return DelegateAgentResult{
		AgentID: agentID,
		RunID:   childRunID,
		Status:  resp.Status,
		Summary: resp.FinalResponse,
	}, nil
}

// spawnTurnOutput is the FunctionResponse map the parent LLM sees.
func spawnTurnOutput(turnID, status string) map[string]any {
	return map[string]any{
		"agent_turn_id": turnID,
		"status":        status,
	}
}
