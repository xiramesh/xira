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
// D-3 (RFC): spawn result payload goes via SpawnBus (polled by poll_turn).
// The old AgentTurnCompleted EventBus signal was removed (Phase 6b, #56).
//
// The ADK tool wrapper (registered in runtimeADKTools via functiontool.New)
// is thin: it builds a spawnSpec + serviceSpawnTarget, calls spawnCore, and
// returns {agent_turn_id, status} as the FunctionResponse map. The real
// logic is in spawnCore.

// spawnTurnToolName is the ADK tool name.
const spawnTurnToolName = "spawn_turn"

// --- child cancel registry (RFC #67: steer cancels outstanding children) ---
//
// ChildCancelRegistry is implemented by the progress package's concrete type
// and injected via ctx. Defining the interface here (in runtime) avoids a
// runtime→progress import cycle (progress imports runtime). When a spawned
// child is registered, spawnCore adds its cancel func so the channel runner
// can cancel all outstanding children when the parent turn is steered.

// ChildCancelRegistry tracks spawned-child cancel funcs by chatKey so a
// steering retry can cancel outstanding children (RFC #67).
type ChildCancelRegistry interface {
	Register(key ChatKey, cancel context.CancelFunc) (unregister func())
	CancelAll(key ChatKey) int
	Reset(key ChatKey)
}

type childCancelRegistryContextKey struct{}

// WithChildCancelRegistry returns a ctx carrying the registry.
func WithChildCancelRegistry(ctx context.Context, reg ChildCancelRegistry) context.Context {
	return context.WithValue(ctx, childCancelRegistryContextKey{}, reg)
}

// ChildCancelRegistryFromContext returns the registry carried in ctx, if any.
func ChildCancelRegistryFromContext(ctx context.Context) (ChildCancelRegistry, bool) {
	reg, ok := ctx.Value(childCancelRegistryContextKey{}).(ChildCancelRegistry)
	return reg, ok
}

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

// PendingResult is what the detached goroutine delivers when the child
// turn finishes. Consumed by the parent turn's wait_turn tool (which blocks
// on the SpawnBus until a given child completes) and, in future, the Phase 4
// checkpoint drain. Exported so the production SpawnBus implementation
// (progress.SpawnCollector) can carry it across the package boundary.
type PendingResult struct {
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
// all parent Values, leaking the parent's SteeringBus (parent interjections
// steered the child). childToolConstraintCtx then selectively re-attaches the
// parent's tool constraints AND EventBus (RFC #66: child progress routes to
// the parent's chat key so users see what a spawned child is doing) while
// keeping the SteeringBus stripped. RunChildAgent re-establishes every
// execution-needed key itself (toolFailureGuard, toolTrace, suspendCollector,
// runExecution, runDir, LLM instrumentation). The timeout bounds the detached
// goroutine so a hanging child cannot leak forever / bill infinitely (C2).
//
// The child result goes to SpawnBus (looked up from parentCtx via
// SpawnBusFromContext). When no sink is present, the result is dropped with
// a Warn log. poll_turn pulls results from the SpawnBus.
//
// onChildDone is invoked exactly once when the goroutine finishes (success,
// failure, OR panic) — used to release the parallel slot reserved by
// evaluateSpawnGuardrails. May be nil.
//
// effectiveTimeoutMS is the child deadline in milliseconds (from
// evaluateSpawnGuardrails). Must be > 0.
func spawnCore(parentCtx context.Context, spec spawnSpec, target spawnTarget, effectiveTimeoutMS int, onChildDone func()) spawnResult {
	turnID := newSpawnTurnID()
	sink := SpawnBusFromContext(parentCtx)

	// Fresh, timeout-bounded context on a clean base. childToolConstraintCtx
	// starts from Background (stripping the parent's SteeringBus — parent
	// interjections must not steer a spawned child) and re-attaches the
	// parent's tool constraints (allowlist / inputAllowlist /
	// nativeToolsDisabled) AND the parent's EventBus (RFC #66: child progress
	// routes to the parent's chat key), so a spawned child under a narrowed
	// flow-step tool set is bound by the same set as delegate_agent. The
	// deadline bounds the goroutine (C2). RunChildAgent rebuilds
	// execution-needed keys itself.
	childCtx, cancel := context.WithTimeout(childToolConstraintCtx(parentCtx), time.Duration(effectiveTimeoutMS)*time.Millisecond)

	// Register the child's cancel with the per-chat-key registry so that when
	// the parent turn is steered, the channel runner can cancel this child
	// (RFC #67). Registered BEFORE the goroutine starts so a steer racing with
	// goroutine startup still sees this child. The goroutine unregisters on
	// exit to prevent the registry slice from growing unboundedly.
	var unregister context.CancelFunc
	if reg, ok := ChildCancelRegistryFromContext(parentCtx); ok {
		if key, ok := ChatKeyFromContext(parentCtx); ok {
			unregister = reg.Register(key, cancel)
		}
	}

	go func() {
		// onChildDone releases the parallel slot. It must run on every exit
		// path including panic, so it is deferred FIRST (outermost), after
		// the panic is recovered.
		if onChildDone != nil {
			defer onChildDone()
		}
		defer cancel()
		// Unregister the child from the steer-cancel registry on every exit
		// path (RFC #67). Without this, the per-key slice would accumulate
		// cancel funcs for every child ever spawned in the conversation.
		if unregister != nil {
			defer unregister()
		}

		// Deliver the (possibly panic-derived) pending result to the SpawnBus.
		// Deferred BEFORE target.Run so a panic still produces a sink delivery —
		// recovering a panic but dropping the result would be a silent-data-loss
		// bug. (The old AgentTurnCompleted EventBus signal was removed in Phase 6b
		// #56 — it was fire-and-nobody-listens; poll_turn pulls results directly
		// from the SpawnBus.)
		deliver := func(pr PendingResult) {
			if sink != nil {
				sink.Deliver(pr)
			} else {
				slog.Warn("spawn_turn child result dropped (no SpawnBus in context)",
					"turn_id", turnID,
					"agent_id", spec.AgentID)
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
				deliver(PendingResult{
					TurnID: turnID,
					Err:    fmt.Sprintf("child agent panicked: %v", r),
				})
			}
		}()

		result, err := target.Run(childCtx, spec.AgentID, spec.Task)

		pr := PendingResult{TurnID: turnID}
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
// Uses the FULL uuid — not uuid[:8] — because the ID is the SpawnCollector
// key (poll_turn looks up by it). A truncated id (65k → 50% collision) would
// cross-link child results (PR #53 review WARNING). Full uuid collides ~never.
func newSpawnTurnID() string {
	return "spawn:" + uuid.NewString()
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
// arrive in Phase 4/5 when SpawnBus consumers do.
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
	// parsing (evidence/limitations/confidence) is the SpawnBus consumer's
	// job (Phase 4/5), not spawn's — spawn is fire-and-forget.
	result := DelegateAgentResult{
		AgentID: agentID,
		RunID:   childRunID,
		Status:  resp.Status,
		Summary: resp.FinalResponse,
	}
	// #68: if the child entered HITL, carry ALL its pending questions +
	// HumanRequestIDs so poll_turn can surface them to the parent LLM (and the
	// parent can answer each via answer_child). A turn can produce >1 HR
	// (multiple human.request calls); carrying all avoids silently dropping the
	// rest (PR #77 follow-up: previously only [0] was carried).
	if resp.Status == StatusWaitingHuman && len(resp.HumanRequests) > 0 {
		pq := make([]PendingQuestion, 0, len(resp.HumanRequests))
		for _, hr := range resp.HumanRequests {
			pq = append(pq, PendingQuestion{Question: hr.Question, HumanRequestID: hr.ID})
		}
		result.PendingQuestions = pq
	}
	return result, nil
}

// spawnTurnOutput is the FunctionResponse map the parent LLM sees.
func spawnTurnOutput(turnID, status string) map[string]any {
	return map[string]any{
		"agent_turn_id": turnID,
		"status":        status,
	}
}

// childToolConstraintCtx returns a context that carries the parent's tool
// constraints AND EventBus on a clean context.Background base.
//
// What the child inherits:
//   - Tool constraints (allowlist / inputAllowlist / nativeToolsDisabled):
//     under a narrowed flow-step tool set, a spawned child is bound by the
//     same set as delegate_agent (whose WithTimeout(ctx, ...) inherits the
//     parent ctx wholesale).
//   - EventBus (RFC #66 / spawn-parent-child-comm-rfc §3): the child's
//     progress events route to the parent's chat key so users can see what a
//     spawned child is doing. This REVERSES the original C3 decision (full
//     isolation cut off a legitimate parent-child link — a long-running child
//     was completely invisible to the user). The child's events carry a
//     distinct AgentTurnID + ParentAgentTurnID, so the renderer can attribute
//     and prefix them by source.
//   - ChatKey + ChildCancelRegistry (RFC #67): so the child can register its
//     cancel func and be canceled when the parent turn is steered.
//
// What the child does NOT inherit:
//   - SteeringBus: parent interjections must NOT steer a spawned child (the
//     child isn't in direct conversation with the user). When the parent is
//     steered, outstanding children are canceled separately (RFC §5 / #67),
//     not steered.
//
// Starting from Background strips everything; we then re-attach only the
// keys above. RunChildAgent re-establishes the remaining execution-needed
// keys (toolFailureGuard / toolTrace / suspendCollector / runExecution /
// runDir / LLM instrumentation) itself, so they need not be carried here.
func childToolConstraintCtx(parent context.Context) context.Context {
	ctx := context.Background()
	if allowed, ok := parent.Value(runtimeToolAllowlistContextKey{}).(map[string]struct{}); ok && len(allowed) > 0 {
		ctx = context.WithValue(ctx, runtimeToolAllowlistContextKey{}, allowed)
	}
	if in, ok := parent.Value(runtimeToolInputAllowlistContextKey{}).(map[string]map[string]map[string]struct{}); ok && len(in) > 0 {
		ctx = context.WithValue(ctx, runtimeToolInputAllowlistContextKey{}, in)
	}
	if disabled, ok := parent.Value(runtimeNativeToolsDisabledContextKey{}).(bool); ok && disabled {
		ctx = context.WithValue(ctx, runtimeNativeToolsDisabledContextKey{}, disabled)
	}
	// EventBus is inherited so child progress routes to the parent's chat key
	// (RFC #66). SteeringBus stays stripped — see the doc comment above.
	if bus := EventBusFromContext(parent); bus != nil {
		ctx = WithEventBus(ctx, bus)
	}
	// ChatKey + ChildCancelRegistry are inherited (RFC #67) so the child can
	// register its cancel func and be canceled on parent steer.
	if key, ok := ChatKeyFromContext(parent); ok {
		ctx = WithChatKey(ctx, key)
	}
	if reg, ok := ChildCancelRegistryFromContext(parent); ok {
		ctx = WithChildCancelRegistry(ctx, reg)
	}
	return ctx
}

// evaluateSpawnGuardrails applies the same delegation guardrails delegate_agent
// uses (delegation.go), so spawn_turn cannot bypass them during grey-rollout
// (where both tools are registered). It MUST be called on the synchronous
// (tool-call) side so rejections are visible to the LLM and a slot is never
// reserved for a rejected spawn.
//
// Guards checked (mirroring the removed delegate_agent path):
//  1. policy.MaxDepth      — requested depth (parentDepth+1) must not exceed it
//  2. policy.MaxOutstanding — outstanding children (in-memory active count)
//     must be below the cap
//  3. policy.MaxParallel    — reserves an active-child slot; the returned
//     release callback frees it (spawn holds the slot for the child's async
//     lifetime)
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
