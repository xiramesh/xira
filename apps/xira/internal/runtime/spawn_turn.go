package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
// The detached goroutine uses a fresh context.Background() with a timeout
// (effectiveTimeoutMS) — NOT the parent ctx. Starting from Background()
// (instead of WithoutCancel(parentCtx)) is deliberate: WithoutCancel copied
// all parent Values, leaking the parent's EventSink (child progress polluted
// the parent IM stream) and SteeringSink (parent interjections steered the
// child). RunChildAgent re-establishes every execution-needed key itself
// (toolFailureGuard, toolTrace, suspendCollector, runExecution, runDir, LLM
// instrumentation), so a clean Background base is safe and isolates the
// child's output sinks. The timeout bounds the detached goroutine so a
// hanging child cannot leak forever / bill infinitely (C2).
//
// The child result goes to SpawnSink (looked up from parentCtx via
// SpawnSinkFromContext). When no sink is present, the result is dropped with
// a Warn log (Phase 3 fire-and-forget — sink consumers arrive in Phase 4/5).
//
// signalBus is optional (nil = no signal published). When present,
// AgentTurnCompleted signal is published on child completion.
//
// onChildDone is invoked exactly once when the goroutine finishes (success,
// failure, OR panic) — used to release the parallel slot reserved by
// evaluateSpawnGuardrails. May be nil.
//
// effectiveTimeoutMS is the child deadline in milliseconds (from
// evaluateSpawnGuardrails). Must be > 0.
func spawnCore(parentCtx context.Context, spec spawnSpec, target spawnTarget, signalBus EventBus, effectiveTimeoutMS int, onChildDone func()) spawnResult {
	turnID := newSpawnTurnID()
	sink := SpawnSinkFromContext(parentCtx)

	// Fresh, timeout-bounded context. Background (not WithoutCancel(parentCtx))
	// strips the parent's EventSink + SteeringSink (C3); the deadline bounds
	// the goroutine (C2). RunChildAgent rebuilds execution-needed keys itself.
	childCtx, cancel := context.WithTimeout(context.Background(), time.Duration(effectiveTimeoutMS)*time.Millisecond)

	go func() {
		// onChildDone releases the parallel slot. It must run on every exit
		// path including panic, so it is deferred FIRST (outermost), after
		// the panic is recovered.
		if onChildDone != nil {
			defer onChildDone()
		}
		defer cancel()

		// Deliver the (possibly panic-derived) pending result + signal.
		// Deferred BEFORE target.Run so a panic still produces a sink/signal
		// delivery — recovering a panic but dropping the result would be a
		// new silent-data-loss bug.
		deliver := func(pr pendingResult) {
			if sink != nil {
				sink.Deliver(pr)
			} else {
				slog.Warn("spawn_turn child result dropped (no SpawnSink in context)",
					"turn_id", turnID,
					"agent_id", spec.AgentID)
			}
			if signalBus != nil {
				signalBus.PublishEvent(AgentTurnCompleted{
					MessageIDVal:   "spawn_complete_" + turnID,
					AgentTurnIDVal: AgentTurnID(turnID),
					TimestampVal:   time.Now(),
					TurnKind:       AgentTurnKindAgent,
				})
			}
		}

		// C1: recover a panicking child so it crashes neither the goroutine
		// nor the process. The panic is converted into a child-failed
		// pendingResult so it is still surfaced (not silently swallowed).
		defer func() {
			if r := recover(); r != nil {
				slog.Error("spawn_turn child panicked",
					"turn_id", turnID,
					"agent_id", spec.AgentID,
					"panic", r)
				deliver(pendingResult{
					TurnID: turnID,
					Err:    fmt.Sprintf("child agent panicked: %v", r),
				})
			}
		}()

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
		deliver(pr)
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

// evaluateSpawnGuardrails applies the same delegation guardrails delegate_agent
// uses (delegation.go), so spawn_turn cannot bypass them during grey-rollout
// (where both tools are registered). It MUST be called on the synchronous
// (tool-call) side so rejections are visible to the LLM and a slot is never
// reserved for a rejected spawn.
//
// Guards checked (mirroring delegate_agent delegation.go):
//  1. policy.MaxDepth      — requested depth (parentDepth+1) must not exceed it
//  2. policy.MaxOutstanding — outstanding children (in-memory + persisted join
//     state) must be below the cap
//  3. policy.MaxParallel    — reserves an active-child slot; the returned
//     release callback frees it (spawn holds the slot for the child's async
//     lifetime, delegate holds it for the tool-call lifetime)
//
// On success it returns a release callback (call EXACTLY once when the child
// goroutine finishes), the effective timeout in ms (policy.DefaultMaxDurationMS
// — spawn has no per-call max_duration_ms input in Phase 3), and nil error.
// On rejection the slot is NOT reserved and release is nil.
//
// effectiveTimeoutMS: spawn's Phase 3 input schema has only agent_id+task, so
// there is no per-call override; the effective timeout is the policy default,
// matching delegate_agent's no-input path (delegation.go effectiveMaxDurationMS).
func evaluateSpawnGuardrails(s *Service, policy agents.DelegationPolicy, parentRunID string, parentDepth int) (release func(), effectiveTimeoutMS int, err error) {
	requestedDepth := parentDepth + 1
	if requestedDepth > policy.MaxDepth {
		return nil, 0, fmt.Errorf("spawn depth %d exceeds max_depth %d", requestedDepth, policy.MaxDepth)
	}
	outstanding, countErr := s.outstandingChildCount(parentRunID)
	if countErr != nil {
		return nil, 0, fmt.Errorf("spawn outstanding check failed: %w", countErr)
	}
	if outstanding >= policy.MaxOutstanding {
		return nil, 0, fmt.Errorf("outstanding child count %d exceeds max_outstanding %d", outstanding, policy.MaxOutstanding)
	}
	if _, reserved := s.reserveChildSlot(parentRunID, policy.MaxParallel); !reserved {
		return nil, 0, fmt.Errorf("active child count exceeds max_parallel %d", policy.MaxParallel)
	}
	// releaseChildSlot is not idempotent (it would underflow on a second
	// call), so guard the release with sync.Once — the goroutine calls it
	// exactly once regardless of how it exits.
	var once sync.Once
	release = func() {
		once.Do(func() { s.releaseChildSlot(parentRunID) })
	}
	return release, policy.DefaultMaxDurationMS, nil
}
