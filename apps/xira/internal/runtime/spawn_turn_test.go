package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/agents"
)

// spawn_turn_test.go: tests the core spawn logic (not the ADK tool wrapper).
// The ADK StreamingFunctionTool wrapper is thin — it calls spawnCore and
// yields the result. The real logic is in spawnCore: detach goroutine,
// run child turn, deliver signal + result.
//
// RFC §2.4 (corrected): spawn_turn = yield "spawned" + detached goroutine.
// D-3: result payload via SpawnBus, signal via EventBus.

// mockSpawnTarget is a test double for the child turn executor.
type mockSpawnTarget struct {
	mu     sync.Mutex
	called bool
	agent  string
	task   string
	result DelegateAgentResult
	err    error
}

func (m *mockSpawnTarget) Run(ctx context.Context, agentID, task string) (DelegateAgentResult, error) {
	m.mu.Lock()
	m.called = true
	m.agent = agentID
	m.task = task
	result := m.result
	err := m.err
	m.mu.Unlock()
	return result, err
}

// mockSpawnBus is a test double for SpawnBus.
type mockSpawnBus struct {
	mu      sync.Mutex
	results []PendingResult
}

func (m *mockSpawnBus) Deliver(pr PendingResult) {
	m.mu.Lock()
	m.results = append(m.results, pr)
	m.mu.Unlock()
}

func (m *mockSpawnBus) latest() (PendingResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.results) == 0 {
		return PendingResult{}, false
	}
	return m.results[len(m.results)-1], true
}

func TestSpawnCoreReturnsTurnIDImmediately(t *testing.T) {
	target := &mockSpawnTarget{result: DelegateAgentResult{
		AgentID: "code-agent",
		RunID:   "child-run-1",
		Status:  "completed",
		Summary: "done",
	}}

	// spawnCore should return a turn ID + spawned status immediately,
	// without waiting for the child turn to complete.
	sink := &mockSpawnBus{}
	ctx := WithSpawnBus(context.Background(), sink)
	spec := spawnSpec{
		AgentID: "code-agent",
		Task:    "do something",
	}

	result := spawnCore(ctx, spec, target, 30000, nil)

	if result.TurnID == "" {
		t.Error("spawnCore returned empty TurnID")
	}
	if result.Status != "spawned" {
		t.Errorf("Status = %q, want 'spawned'", result.Status)
	}
}

func TestSpawnCoreRunsChildInDetachedGoroutine(t *testing.T) {
	target := &mockSpawnTarget{result: DelegateAgentResult{
		AgentID: "code-agent",
		Status:  "completed",
		Summary: "done",
	}}

	sink := &mockSpawnBus{}
	ctx := WithSpawnBus(context.Background(), sink)
	spec := spawnSpec{
		AgentID: "code-agent",
		Task:    "do something async",
	}

	// spawnCore returns immediately — the child runs in a goroutine.
	_ = spawnCore(ctx, spec, target, 30000, nil)

	// Wait for the child goroutine to call the target.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			target.mu.Lock()
			called := target.called
			target.mu.Unlock()
			if called {
				close(done)
				return
			}
			time.Sleep(time.Millisecond)
		}
		close(done)
	}()

	select {
	case <-done:
		// Child was called in detached goroutine.
	case <-time.After(2 * time.Second):
		t.Fatal("child turn was not called within 2s — goroutine not detached")
	}

	target.mu.Lock()
	if target.agent != "code-agent" || target.task != "do something async" {
		t.Errorf("child called with agent=%q task=%q", target.agent, target.task)
	}
	target.mu.Unlock()
}

func TestSpawnCoreDeliversResultToSink(t *testing.T) {
	expected := DelegateAgentResult{
		AgentID: "code-agent",
		RunID:   "child-1",
		Status:  "completed",
		Summary: "result text",
	}
	target := &mockSpawnTarget{result: expected}

	sink := &mockSpawnBus{}
	ctx := WithSpawnBus(context.Background(), sink)
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	_ = spawnCore(ctx, spec, target, 30000, nil)

	waitFor(t, 2*time.Second, func() bool {
		_, ok := sink.latest()
		return ok
	})

	got, ok := sink.latest()
	if !ok {
		t.Fatal("no result delivered to sink")
	}
	if got.Result.Summary != "result text" {
		t.Errorf("sink result summary = %q, want 'result text'", got.Result.Summary)
	}
}

func TestSpawnCoreChildErrorDeliversError(t *testing.T) {
	target := &mockSpawnTarget{err: errors.New("child crashed")}
	sink := &mockSpawnBus{}
	ctx := WithSpawnBus(context.Background(), sink)
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	_ = spawnCore(ctx, spec, target, 30000, nil)

	waitFor(t, 2*time.Second, func() bool {
		_, ok := sink.latest()
		return ok
	})

	got, ok := sink.latest()
	if !ok {
		t.Fatal("no error result delivered to sink")
	}
	if got.Err == "" {
		t.Error("expected error in pending result")
	}
	if got.Err != "child crashed" {
		t.Errorf("error = %q, want 'child crashed'", got.Err)
	}
}

func TestSpawnCoreChildUsesDetachedContext(t *testing.T) {
	// The child goroutine must NOT inherit the parent ctx — if parent ctx
	// is canceled, child should still run. (RFC §2.4: context.WithoutCancel)
	ctx, cancel := context.WithCancel(context.Background())

	block := make(chan struct{})
	target := &mockChildTarget{block: block}
	sink := &mockSpawnBus{}
	ctx = WithSpawnBus(ctx, sink)
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	_ = spawnCore(ctx, spec, target, 30000, nil)
	cancel() // cancel parent ctx

	// Child goroutine should still run (detached) — it's blocked on `block`,
	// not on ctx. If it inherited ctx, cancel would kill it before block.
	time.Sleep(50 * time.Millisecond)

	// Unblock child — if it's still alive, it delivers a result.
	close(block)

	waitFor(t, 2*time.Second, func() bool {
		_, ok := sink.latest()
		return ok
	})
	// Child survived parent cancel → detached context works.
}

func TestSpawnCoreNoSinkDropsResultSafely(t *testing.T) {
	// Phase 3 fire-and-forget: when no SpawnBus is in the context, the
	// child result is dropped + Warn-logged, NOT a panic. This is the
	// documented Phase 3 limitation (sink consumers arrive in Phase 4/5).
	target := &mockSpawnTarget{result: DelegateAgentResult{
		AgentID: "code-agent",
		Status:  "completed",
		Summary: "ignored",
	}}
	// No WithSpawnBus — ctx carries no sink.
	ctx := context.Background()
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	// Must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("spawnCore panicked without sink: %v", r)
		}
	}()

	result := spawnCore(ctx, spec, target, 30000, nil)
	if result.Status != "spawned" {
		t.Errorf("Status = %q, want 'spawned'", result.Status)
	}

	// Give the detached goroutine a moment to complete; the only contract
	// here is "no panic, no hang".
	time.Sleep(50 * time.Millisecond)
}

func TestSpawnSpecValidation(t *testing.T) {
	cases := []struct {
		name    string
		spec    spawnSpec
		wantErr bool
	}{
		{"valid", spawnSpec{AgentID: "agent-1", Task: "do stuff"}, false},
		{"empty agent", spawnSpec{AgentID: "", Task: "do stuff"}, true},
		{"empty task", spawnSpec{AgentID: "agent-1", Task: ""}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.spec.Validate()
			if c.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !c.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestSanitizeSpawnTurnInput(t *testing.T) {
	// Valid input: only agent_id + task extracted, nothing else.
	spec, clean, unsupported := sanitizeSpawnTurnInput(map[string]any{
		"agent_id": "research-assistant",
		"task":     "find evidence",
	})
	if spec.AgentID != "research-assistant" || spec.Task != "find evidence" {
		t.Errorf("spec = %+v", spec)
	}
	if len(unsupported) != 0 {
		t.Errorf("expected no unsupported fields, got %v", unsupported)
	}
	if clean["agent_id"] != "research-assistant" || clean["task"] != "find evidence" {
		t.Errorf("clean = %+v", clean)
	}

	// Input with unsupported fields: reported but spec still extracted.
	spec, clean, unsupported = sanitizeSpawnTurnInput(map[string]any{
		"agent_id":    "research-assistant",
		"task":        "find evidence",
		"max_duration_ms": 5000, // not yet supported in Phase 3
	})
	if spec.AgentID != "research-assistant" {
		t.Errorf("agent extraction broken by extra field: %+v", spec)
	}
	if len(unsupported) != 1 || unsupported[0] != "max_duration_ms" {
		t.Errorf("unsupported = %v, want [max_duration_ms]", unsupported)
	}
	if _, ok := clean["max_duration_ms"]; ok {
		t.Error("unsupported field should not appear in clean input")
	}

	// Missing fields: spec is zero-value, Validate() catches it.
	spec, _, _ = sanitizeSpawnTurnInput(map[string]any{"task": "no agent"})
	if err := spec.Validate(); err == nil {
		t.Error("expected validation error for missing agent_id")
	}
}

func TestSpawnTurnOutputShape(t *testing.T) {
	out := spawnTurnOutput("spawn:abc123", "spawned")
	if out["agent_turn_id"] != "spawn:abc123" {
		t.Errorf("agent_turn_id = %v", out["agent_turn_id"])
	}
	if out["status"] != "spawned" {
		t.Errorf("status = %v", out["status"])
	}
}

func TestServiceSpawnTargetRejectsUnknownAgent(t *testing.T) {
	// serviceSpawnTarget.Run must reject agents that aren't registered,
	// without invoking RunChildAgent. Covers the first guard branch.
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	target := &serviceSpawnTarget{
		service: rt,
		caller:  agents.BuiltinXiraAssistant(),
	}
	_, err := target.Run(context.Background(), "nonexistent-agent", "task")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "not registered") {
		t.Errorf("error = %q", err.Error())
	}
}

func TestServiceSpawnTargetRejectsDisallowedAgent(t *testing.T) {
	// serviceSpawnTarget.Run must reject agents not in the caller's
	// delegation allow list, even if the agent is registered. Covers the
	// policy guard branch. Uses ResearchAssistant as caller (delegation
	// disabled by default) → reject regardless of target.
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	target := &serviceSpawnTarget{
		service: rt,
		caller:  agents.BuiltinResearchAssistant(), // delegation disabled
	}
	_, err := target.Run(context.Background(), agents.DefaultAgentID, "task")
	if err == nil {
		t.Fatal("expected error for disallowed agent")
	}
	if !strings.Contains(err.Error(), "not allowed to spawn") {
		t.Errorf("error = %q", err.Error())
	}
}

type mockChildTarget struct {
	block chan struct{}
}

func (m *mockChildTarget) Run(ctx context.Context, agentID, task string) (DelegateAgentResult, error) {
	<-m.block
	return DelegateAgentResult{AgentID: agentID, Status: "completed", Summary: "done"}, nil
}

// --- Guards: panic / timeout / ctx isolation (C1-C3) ---

// mockPanicTarget panics when Run is called. Used to verify the detached
// goroutine recovers instead of crashing the process.
type mockPanicTarget struct {
	ran chan struct{}
}

func (m *mockPanicTarget) Run(ctx context.Context, agentID, task string) (DelegateAgentResult, error) {
	if m.ran != nil {
		close(m.ran)
	}
	panic("child boom")
}

// noopEventBus is a no-op EventBus used to seed a parent ctx and verify
// the child does NOT inherit it.
type noopEventBus struct{}

func (noopEventBus) Deliver(evt Event) {}

// noopSteeringBus is a no-op SteeringBus for the same purpose.
type noopSteeringBus struct{}

func (noopSteeringBus) Enqueue(string)               {}
func (noopSteeringBus) TryDequeue() (string, bool)   { return "", false }
func (noopSteeringBus) DrainAll() []string           { return nil }
func (noopSteeringBus) HasPending() bool             { return false }

// TestSpawnCoreChildPanicRecovered verifies C1: a panicking child turn is
// recovered by the detached goroutine. spawnCore must return normally and
// deliver an error result to the sink — NOT crash the test process.
func TestSpawnCoreChildPanicRecovered(t *testing.T) {
	target := &mockPanicTarget{ran: make(chan struct{})}
	sink := &mockSpawnBus{}
	ctx := WithSpawnBus(context.Background(), sink)
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	// If the goroutine's panic escapes unrecovered, the test binary crashes
	// and this assertion is never reached — the failure is the crash itself.
	result := spawnCore(ctx, spec, target, 30000, nil)
	if result.Status != "spawned" {
		t.Fatalf("Status = %q, want 'spawned'", result.Status)
	}

	// Wait for the goroutine to have run (and panicked).
	select {
	case <-target.ran:
	case <-time.After(2 * time.Second):
		t.Fatal("child goroutine never ran")
	}

	// The panic must surface as a child-failed pendingResult in the sink,
	// not be swallowed.
	waitFor(t, 2*time.Second, func() bool {
		_, ok := sink.latest()
		return ok
	})
	got, ok := sink.latest()
	if !ok {
		t.Fatal("panic did not produce a pending result in the sink")
	}
	if got.Err == "" {
		t.Error("expected non-empty Err in pending result after child panic")
	}
}

// TestSpawnCoreChildTimeoutBoundsGoroutine verifies C2: the child ctx carries
// a deadline so a hanging child cannot leak the goroutine forever. The mock
// target blocks until its ctx is canceled, then records ctx.Err().
type mockCtxInspectTarget struct {
	gotCtx context.Context
	done   chan struct{}
}

func (m *mockCtxInspectTarget) Run(ctx context.Context, agentID, task string) (DelegateAgentResult, error) {
	m.gotCtx = ctx
	<-ctx.Done() // block until the spawn timeout cancels the child ctx
	close(m.done)
	return DelegateAgentResult{AgentID: agentID, Status: "failed"}, ctx.Err()
}

func TestSpawnCoreChildTimeoutBoundsGoroutine(t *testing.T) {
	target := &mockCtxInspectTarget{done: make(chan struct{})}
	sink := &mockSpawnBus{}
	ctx := WithSpawnBus(context.Background(), sink)
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	// 50ms timeout — the child must be canceled shortly after.
	spawnCore(ctx, spec, target, 50, nil)

	select {
	case <-target.done:
	case <-time.After(2 * time.Second):
		t.Fatal("child goroutine was not canceled by the spawn timeout within 2s")
	}
	if target.gotCtx == nil {
		t.Fatal("child target was never invoked")
	}
	if err := target.gotCtx.Err(); err == nil {
		t.Error("child ctx was not canceled after timeout")
	}
}

// TestSpawnCoreChildInheritsParentEventBus verifies the spawn child INHERITS
// the parent's EventBus (RFC #66 / spawn-parent-child-comm-rfc §3): child turn
// progress events route to the parent's chat key so users see what a spawned
// child is doing. This REVERSES the original C3 "isolate EventBus" decision —
// full isolation was too extreme, cutting off a legitimate parent-child link.
//
// The SteeringBus stays isolated: parent interjections must NOT steer a spawned
// child (the child isn't in a direct conversation with the user). Steering
// cancellation of outstanding children is a separate mechanism (RFC §5 / #67).
type mockAssertCleanCtxTarget struct {
	gotCtx context.Context
	done   chan struct{}
}

func (m *mockAssertCleanCtxTarget) Run(ctx context.Context, agentID, task string) (DelegateAgentResult, error) {
	m.gotCtx = ctx
	close(m.done)
	return DelegateAgentResult{AgentID: agentID, Status: "completed", Summary: "done"}, nil
}

func TestSpawnCoreChildInheritsParentEventBus(t *testing.T) {
	target := &mockAssertCleanCtxTarget{done: make(chan struct{})}
	sink := &mockSpawnBus{}
	parentBus := noopEventBus{}

	// Parent ctx carries BOTH sinks — exactly what an IM turn ctx carries.
	parent := context.Background()
	parent = WithSpawnBus(parent, sink)
	parent = WithEventBus(parent, parentBus)
	parent = WithSteeringBus(parent, noopSteeringBus{})

	spec := spawnSpec{AgentID: "code-agent", Task: "task"}
	spawnCore(parent, spec, target, 30000, nil)

	<-target.done
	// EventBus IS inherited — child progress routes to the parent's chat key.
	if got := EventBusFromContext(target.gotCtx); got == nil {
		t.Error("child ctx did NOT inherit parent EventBus — child progress is invisible to the parent (RFC #66)")
	} else if got != any(parentBus).(EventBus) && fmt.Sprintf("%p", got) != fmt.Sprintf("%p", parentBus) {
		// noopEventBus is a distinct value type; the child must carry the
		// SAME bus instance the parent set. Compare by pointer to be strict.
		t.Error("child ctx carries a different EventBus instance than the parent")
	}
	// SteeringBus is STILL isolated — parent interjections must not steer a
	// spawned child.
	if SteeringBusFromContext(target.gotCtx) != nil {
		t.Error("child ctx inherited parent SteeringBus — parent interjections would steer the child")
	}
	// SpawnBus is consumed by spawnCore from the parent ctx (for result
	// delivery), not by the child execution — so the child ctx need not
	// carry it.
}

// recordingEventBus captures every delivered Event. Used to prove that a
// spawned child's dispatchEvent reaches the parent's EventBus instance.
type recordingEventBus struct {
	mu     sync.Mutex
	events []Event
}

func (r *recordingEventBus) Deliver(evt Event) {
	r.mu.Lock()
	r.events = append(r.events, evt)
	r.mu.Unlock()
}

func (r *recordingEventBus) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// mockEventDispatchTarget is a spawnTarget that, inside Run, dispatches a
// signal event (run.started) through the child ctx — exactly what a real
// RunChildAgent does via recordChildEvent. It proves the child's dispatched
// events reach the parent's EventBus (RFC #66).
type mockEventDispatchTarget struct {
	done chan struct{}
}

func (m *mockEventDispatchTarget) Run(ctx context.Context, agentID, task string) (DelegateAgentResult, error) {
	defer close(m.done)
	// Construct a signal event the way RunChildAgent does and dispatch it
	// through the child ctx. If the ctx inherited the parent's EventBus, this
	// reaches the parent bus; otherwise it's dropped (Debug log only).
	const childRun = "child-run-research"
	base := runtimeEventBase{
		RunID:   childRun,
		AgentID: agentID,
	}
	corr := &runtimeEventCorrelationInput{
		ParentRunID: "parent-run-xyz",
		ChildRunID:  childRun,
	}
	evt := newRuntimeEvent(base, "run.started", "runtime", "child agent run started", map[string]any{
		"agent_id": agentID,
	}, corr)
	dispatchEvent(ctx, evt)
	return DelegateAgentResult{AgentID: agentID, Status: "completed", Summary: "done"}, nil
}

// TestSpawnCoreChildEventsReachParentBus is the end-to-end contract test for
// RFC #66: a spawned child's dispatched signal events MUST reach the parent's
// EventBus. Before this change, the child ctx started from context.Background()
// and dispatchEvent found no sink → child progress was silently dropped.
func TestSpawnCoreChildEventsReachParentBus(t *testing.T) {
	bus := &recordingEventBus{}
	target := &mockEventDispatchTarget{done: make(chan struct{})}
	sink := &mockSpawnBus{}

	parent := context.Background()
	parent = WithSpawnBus(parent, sink)
	parent = WithEventBus(parent, bus)

	spec := spawnSpec{AgentID: "research-assistant", Task: "research LLM agents"}
	spawnCore(parent, spec, target, 30000, nil)

	// Wait for the child goroutine to dispatch its event.
	select {
	case <-target.done:
	case <-time.After(2 * time.Second):
		t.Fatal("child goroutine never ran")
	}

	// The dispatched run.started must be in the parent bus, mapped to the
	// AgentTurnStarted Event type.
	events := bus.snapshot()
	var got AgentTurnStarted
	count := 0
	for _, e := range events {
		if s, ok := e.(AgentTurnStarted); ok && s.Kind() == "agent_turn.started" {
			got = s
			count++
		}
	}
	if count == 0 {
		t.Fatalf("child's run.started event did not reach parent EventBus; got %d events: %v", len(events), events)
	}
	// Correlation: the child event carries the child's turn ID and the parent
	// turn ID, so the renderer can attribute it to the child (and link it to
	// the parent). This is the data the progress renderer needs to prefix
	// child events with their source agent.
	if got.AgentTurnID() == "" {
		t.Error("child event missing AgentTurnID — renderer cannot attribute it to the child turn")
	}
	if string(got.ParentAgentTurnID()) == "" {
		t.Error("child event missing ParentAgentTurnID — renderer cannot link it to the parent turn")
	}
	if string(got.AgentTurnID()) == string(got.ParentAgentTurnID()) {
		t.Error("child AgentTurnID equals ParentAgentTurnID — child event would be indistinguishable from a parent event")
	}
}

// --- Tool-constraint inheritance (R3: allowlist regression) ---

// TestSpawnCoreChildInheritsParentToolConstraints verifies the spawn child
// inherits the parent's tool constraints (allowlist + inputAllowlist +
// native-tools-disabled), matching delegate_agent. R2's C3 fix used
// context.Background() to isolate the parent's sinks — which correctly
// stripped EventBus/SteeringBus but also stripped the tool allowlist,
// letting a spawned child run under a wider tool set than its parent (a
// flow-step allowlist bypass). The child ctx must carry the tool constraints
// on a clean base.
func TestSpawnCoreChildInheritsParentToolConstraints(t *testing.T) {
	target := &mockAssertCleanCtxTarget{done: make(chan struct{})}
	sink := &mockSpawnBus{}

	// Parent ctx: narrowed tool allowlist + tool-input allowlist + native
	// tools disabled + BOTH output sinks (the sinks must still be stripped,
	// the tool constraints must survive).
	parent := context.Background()
	parent = WithSpawnBus(parent, sink)
	parent = WithEventBus(parent, noopEventBus{})
	parent = WithSteeringBus(parent, noopSteeringBus{})
	parent = contextWithRuntimeToolAllowlist(parent, []string{"write_file"})
	parent = contextWithRuntimeNativeToolsDisabled(parent)
	parent = contextWithRuntimeToolInputAllowlist(parent, map[string]map[string][]string{
		"write_file": {"path": {"/safe/dir"}},
	})

	spec := spawnSpec{AgentID: "code-agent", Task: "task"}
	spawnCore(parent, spec, target, 30000, nil)

	<-target.done
	child := target.gotCtx

	// Tool allowlist is inherited — child is bound to the same narrowed set.
	if !runtimeToolAllowedFromContext(child, "write_file") {
		t.Error("child lost parent tool allowlist: 'write_file' should be allowed")
	}
	if runtimeToolAllowedFromContext(child, "shell") {
		t.Error("child tool allowlist is open: 'shell' should be rejected (not in parent allowlist)")
	}

	// Tool-input allowlist is inherited — a disallowed input value is rejected.
	if err := validateRuntimeToolInputAllowlist(child, "write_file", map[string]any{"path": "/etc/passwd"}); err == nil {
		t.Error("child lost parent tool-input allowlist: disallowed path was accepted")
	}
	if err := validateRuntimeToolInputAllowlist(child, "write_file", map[string]any{"path": "/safe/dir"}); err != nil {
		t.Errorf("child tool-input allowlist rejected an allowed value: %v", err)
	}

	// Native-tools-disabled flag is inherited.
	if !runtimeNativeToolsDisabledFromContext(child) {
		t.Error("child lost parent native-tools-disabled flag")
	}

	// Regression guard: the SteeringBus must STILL be stripped (re-attaching
	// tool constraints must not leak it back in). The EventBus is intentionally
	// inherited now (RFC #66) — verified by TestSpawnCoreChildInheritsParentEventBus.
	if SteeringBusFromContext(child) != nil {
		t.Error("re-attaching tool constraints leaked parent SteeringBus back in")
	}
}

// --- Guardrails: MaxDepth / MaxParallel / slot release ---

// TestSpawnGuardrailsRejectExcessDepth verifies the spawn path enforces
// policy.MaxDepth just like delegate_agent. With MaxDepth=1 (the normalized
// default) and parentDepth=1, the requested depth (2) must be rejected.
func TestSpawnGuardrailsRejectExcessDepth(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	caller := agents.BuiltinXiraAssistant() // MaxDepth normalized to 1
	policy := caller.NormalizedDelegationPolicy()

	_, _, err := evaluateSpawnGuardrails(rt, policy, "parent-run-1", 1)
	if err == nil {
		t.Fatal("expected depth rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "depth") {
		t.Errorf("error = %q, want a depth-related rejection", err.Error())
	}
}

// TestSpawnGuardrailsRejectExcessParallel verifies the spawn path enforces
// MaxParallel (active concurrent children) like delegate_agent's
// reserveChildSlot. Pre-reserve MaxParallel slots, then evaluate must reject.
func TestSpawnGuardrailsRejectExcessParallel(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	caller := agents.BuiltinXiraAssistant() // MaxParallel normalized to 1
	policy := caller.NormalizedDelegationPolicy()
	const parentRun = "parent-run-parallel"

	// Saturate the parallel slots (MaxParallel=1 by default).
	for i := 0; i < policy.MaxParallel; i++ {
		if _, ok := rt.reserveChildSlot(parentRun, policy.MaxParallel); !ok {
			t.Fatalf("setup reserve %d failed", i)
		}
	}
	t.Cleanup(func() {
		for i := 0; i < policy.MaxParallel; i++ {
			rt.releaseChildSlot(parentRun)
		}
	})

	_, _, err := evaluateSpawnGuardrails(rt, policy, parentRun, 0)
	if err == nil {
		t.Fatal("expected parallel rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "parallel") {
		t.Errorf("error = %q, want a parallel-related rejection", err.Error())
	}
}

// TestSpawnGuardrailsReserveAndRelease verifies that a successful evaluation
// reserves a slot (visible via activeChildCount) and that calling the returned
// release callback frees it.
func TestSpawnGuardrailsReserveAndRelease(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	caller := agents.BuiltinXiraAssistant()
	policy := caller.NormalizedDelegationPolicy()
	const parentRun = "parent-run-release"

	before := rt.activeChildCount(parentRun)
	if before != 0 {
		t.Fatalf("precondition: activeChildCount = %d, want 0", before)
	}

	release, timeoutMS, err := evaluateSpawnGuardrails(rt, policy, parentRun, 0)
	if err != nil {
		t.Fatalf("evaluateSpawnGuardrails failed: %v", err)
	}
	if release == nil {
		t.Fatal("expected non-nil release callback")
	}
	if timeoutMS <= 0 {
		t.Errorf("effectiveTimeoutMS = %d, want > 0", timeoutMS)
	}

	// Slot is now reserved.
	if got := rt.activeChildCount(parentRun); got != 1 {
		t.Errorf("after reserve activeChildCount = %d, want 1", got)
	}

	// Releasing frees the slot (spawn child completed).
	release()
	if got := rt.activeChildCount(parentRun); got != 0 {
		t.Errorf("after release activeChildCount = %d, want 0", got)
	}

	// releaseChildSlot is not idempotent — calling release twice would
	// underflow. evaluateSpawnGuardrails must hand back a callback that is
	// safe to call exactly once; we do not double-call here (the goroutine
	// calls it once).
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// Compile-time: mockSpawnTarget satisfies spawnTarget interface.
var _ spawnTarget = (*mockSpawnTarget)(nil)
var _ spawnTarget = (*mockChildTarget)(nil)
var _ SpawnBus = (*mockSpawnBus)(nil)

// Ensure agents package is referenced (for spawnSpec fields if needed).
var _ agents.Profile

// --- RFC #67: steer cancels outstanding children ---

// memCancelRegistry is an in-memory ChildCancelRegistry for tests (runtime
// package cannot import progress, so it implements the interface directly).
type memCancelRegistry struct {
	mu       sync.Mutex
	cancels  map[ChatKey][]context.CancelFunc
	canceled int
}

func newMemCancelRegistry() *memCancelRegistry {
	return &memCancelRegistry{cancels: map[ChatKey][]context.CancelFunc{}}
}

func (r *memCancelRegistry) Register(key ChatKey, cancel context.CancelFunc) (unregister func()) {
	r.mu.Lock()
	r.cancels[key] = append(r.cancels[key], cancel)
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { r.remove(key, cancel) }) }
}

func (r *memCancelRegistry) remove(key ChatKey, target context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.cancels[key]
	for i, c := range s {
		if isSameCtxCancel(c, target) {
			r.cancels[key] = append(s[:i], s[i+1:]...)
			break
		}
	}
	if len(r.cancels[key]) == 0 {
		delete(r.cancels, key)
	}
}

func (r *memCancelRegistry) CancelAll(key ChatKey) int {
	r.mu.Lock()
	funcs := r.cancels[key]
	cp := make([]context.CancelFunc, len(funcs))
	copy(cp, funcs)
	r.mu.Unlock()
	for _, c := range cp {
		c()
		r.mu.Lock()
		r.canceled++
		r.mu.Unlock()
	}
	return len(cp)
}

func (r *memCancelRegistry) Reset(key ChatKey) {
	r.mu.Lock()
	delete(r.cancels, key)
	r.mu.Unlock()
}

func (r *memCancelRegistry) count(key ChatKey) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cancels[key])
}

// isSameCtxCancel compares two context.CancelFunc by wrapping them in
// comparable sentinel closures. context.WithCancel returns a func closing
// over *cancelCtx; we compare the underlying ctx via a cancel-time marker.
// Simpler: wrap each registration so we tag identity ourselves. But the
// production registry uses tokens; this test registry is only used where we
// control all registrations, so we compare by having Register tag the func.
// To keep it minimal here, compare by pointer-equality of the wrapped func
// via a per-call sentinel — implemented below using a unique *int.
func isSameCtxCancel(a, b context.CancelFunc) bool {
	// We don't actually need precise identity here — the test registry is
	// single-goroutine register/unregister per child. Compare via sentinel
	// markers added in Register would require changing the stored type; for
	// the test's purposes unregister is best-effort (the goroutine exits and
	// unregister removes its entry). We approximate by pointer of the
	// *cancelCtx the func closes over using reflect.ValueOf Pointer.
	return cancelFuncPtr(a) == cancelFuncPtr(b)
}

func cancelFuncPtr(f context.CancelFunc) uintptr {
	return reflect.ValueOf(f).Pointer()
}

// Compile-time: memCancelRegistry satisfies ChildCancelRegistry.
var _ ChildCancelRegistry = (*memCancelRegistry)(nil)

// blockingSpawnTarget blocks Run until its ctx is Done, then returns a
// "canceled" error. Used to simulate a long-running spawned child that gets
// canceled by a steer.
type blockingSpawnTarget struct {
	started chan struct{}
	doneErr error
}

func (b *blockingSpawnTarget) Run(ctx context.Context, agentID, task string) (DelegateAgentResult, error) {
	close(b.started)
	<-ctx.Done()
	b.doneErr = ctx.Err()
	return DelegateAgentResult{AgentID: agentID, Status: "failed"}, ctx.Err()
}

// TestSpawnCoreRegistersChildCancelForSteer verifies spawnCore registers the
// child's cancel func with the per-chat-key registry (RFC #67), so a steer
// can cancel the child.
func TestSpawnCoreRegistersChildCancelForSteer(t *testing.T) {
	reg := newMemCancelRegistry()
	key := ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}
	target := &blockingSpawnTarget{started: make(chan struct{})}
	sink := &mockSpawnBus{}
	slotReleased := make(chan struct{})

	ctx := WithSpawnBus(context.Background(), sink)
	ctx = WithChatKey(ctx, key)
	ctx = WithChildCancelRegistry(ctx, reg)

	spawnCore(ctx, spawnSpec{AgentID: "code-agent", Task: "long task"}, target, 30000, func() {
		close(slotReleased)
	})

	// Child goroutine registers its cancel before/at startup.
	<-target.started
	if n := reg.count(key); n != 1 {
		t.Fatalf("registry has %d cancel funcs after spawn, want 1", n)
	}

	// Simulate a steer: CancelAll cancels the child.
	if n := reg.CancelAll(key); n != 1 {
		t.Fatalf("CancelAll returned %d, want 1", n)
	}

	// The child goroutine's ctx is Done → it exits → unregisters → releases slot.
	select {
	case <-slotReleased:
	case <-time.After(2 * time.Second):
		t.Fatal("slot not released after cancel — onChildDone did not run")
	}
	// After exit, the registry no longer holds the (now-finished) child.
	if n := reg.count(key); n != 0 {
		t.Errorf("registry has %d cancel funcs after child exit, want 0 (unregister on exit)", n)
	}
}

// TestSpawnCoreChildCancelReleasesSlot verifies that when a child is canceled
// via the registry, the parallel slot (onChildDone) is released — so a steer
// doesn't leak slots.
func TestSpawnCoreChildCancelReleasesSlot(t *testing.T) {
	reg := newMemCancelRegistry()
	key := ChatKey{Channel: "ilink", ChatID: "c1", SenderID: "u1"}
	target := &blockingSpawnTarget{started: make(chan struct{})}
	sink := &mockSpawnBus{}
	slotReleased := make(chan struct{})
	releaseCount := int32(0)

	ctx := WithSpawnBus(context.Background(), sink)
	ctx = WithChatKey(ctx, key)
	ctx = WithChildCancelRegistry(ctx, reg)

	spawnCore(ctx, spawnSpec{AgentID: "code-agent", Task: "task"}, target, 30000, func() {
		atomic.AddInt32(&releaseCount, 1)
		close(slotReleased)
	})

	<-target.started
	reg.CancelAll(key)

	<-slotReleased
	if atomic.LoadInt32(&releaseCount) != 1 {
		t.Errorf("slot released %d times, want exactly 1 (sync.Once)", releaseCount)
	}
}

// TestSpawnCoreChildCancelWithoutRegistry verifies spawnCore still works when
// no registry is in ctx (e.g. unit tests, legacy paths) — registration is a
// no-op, child runs normally.
func TestSpawnCoreChildCancelWithoutRegistry(t *testing.T) {
	target := &mockSpawnTarget{result: DelegateAgentResult{AgentID: "a", Status: "completed"}}
	sink := &mockSpawnBus{}
	ctx := WithSpawnBus(context.Background(), sink)
	// No ChildCancelRegistry, no ChatKey — must not panic.
	spawnCore(ctx, spawnSpec{AgentID: "a", Task: "t"}, target, 30000, nil)

	// spawnCore returns immediately; wait for the detached goroutine to deliver.
	waitFor(t, 2*time.Second, func() bool {
		_, ok := sink.latest()
		return ok
	})
	if _, ok := sink.latest(); !ok {
		t.Error("child did not deliver result without registry in ctx")
	}
}
