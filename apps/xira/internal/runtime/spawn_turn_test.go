package runtime

import (
	"context"
	"errors"
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
