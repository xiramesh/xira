package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
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
// D-3: result payload via SpawnSink, signal via EventBus.

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

// mockSpawnSink is a test double for SpawnSink.
type mockSpawnSink struct {
	mu      sync.Mutex
	results []pendingResult
}

func (m *mockSpawnSink) Deliver(pr pendingResult) {
	m.mu.Lock()
	m.results = append(m.results, pr)
	m.mu.Unlock()
}

func (m *mockSpawnSink) latest() (pendingResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.results) == 0 {
		return pendingResult{}, false
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
	sink := &mockSpawnSink{}
	ctx := WithSpawnSink(context.Background(), sink)
	spec := spawnSpec{
		AgentID: "code-agent",
		Task:    "do something",
	}

	result := spawnCore(ctx, spec, target, nil)

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

	sink := &mockSpawnSink{}
	ctx := WithSpawnSink(context.Background(), sink)
	spec := spawnSpec{
		AgentID: "code-agent",
		Task:    "do something async",
	}

	// spawnCore returns immediately — the child runs in a goroutine.
	_ = spawnCore(ctx, spec, target, nil)

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

	sink := &mockSpawnSink{}
	ctx := WithSpawnSink(context.Background(), sink)
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	_ = spawnCore(ctx, spec, target, nil)

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
	sink := &mockSpawnSink{}
	ctx := WithSpawnSink(context.Background(), sink)
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	_ = spawnCore(ctx, spec, target, nil)

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
	sink := &mockSpawnSink{}
	ctx = WithSpawnSink(ctx, sink)
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	_ = spawnCore(ctx, spec, target, nil)
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
	// Phase 3 fire-and-forget: when no SpawnSink is in the context, the
	// child result is dropped + Warn-logged, NOT a panic. This is the
	// documented Phase 3 limitation (sink consumers arrive in Phase 4/5).
	target := &mockSpawnTarget{result: DelegateAgentResult{
		AgentID: "code-agent",
		Status:  "completed",
		Summary: "ignored",
	}}
	// No WithSpawnSink — ctx carries no sink.
	ctx := context.Background()
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	// Must not panic.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("spawnCore panicked without sink: %v", r)
		}
	}()

	result := spawnCore(ctx, spec, target, nil)
	if result.Status != "spawned" {
		t.Errorf("Status = %q, want 'spawned'", result.Status)
	}

	// Give the detached goroutine a moment to complete; the only contract
	// here is "no panic, no hang".
	time.Sleep(50 * time.Millisecond)
}

func TestSpawnCorePublishesCompletionSignalOnBus(t *testing.T) {
	// D-3: when a signalBus is provided, spawnCore's detached goroutine
	// publishes AgentTurnCompleted (no payload) on child completion.
	bus := NewEventBus()
	t.Cleanup(bus.Close)

	ch := bus.SubscribeFiltered(Filter{})

	target := &mockSpawnTarget{result: DelegateAgentResult{
		AgentID: "code-agent",
		Status:  "completed",
		Summary: "done",
	}}
	sink := &mockSpawnSink{}
	ctx := WithSpawnSink(context.Background(), sink)
	spec := spawnSpec{AgentID: "code-agent", Task: "task"}

	spawned := spawnCore(ctx, spec, target, bus)

	select {
	case got := <-ch:
		completed, ok := got.(AgentTurnCompleted)
		if !ok {
			t.Fatalf("expected AgentTurnCompleted, got %T", got)
		}
		if completed.AgentTurnIDVal != AgentTurnID(spawned.TurnID) {
			t.Errorf("turn id = %q, want %q", completed.AgentTurnIDVal, spawned.TurnID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AgentTurnCompleted signal not published within 2s")
	}
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
var _ SpawnSink = (*mockSpawnSink)(nil)

// Ensure agents package is referenced (for spawnSpec fields if needed).
var _ agents.Profile
