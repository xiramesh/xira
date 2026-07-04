package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
)

// spawn_live_test.go: live (real DeepSeek LLM) tests for spawn_turn + poll_turn.
// These exercise the REAL ADK loop + REAL LLM + REAL spawn goroutine + REAL
// SpawnCollector — the integration path that unit tests (stubbed LLM) cannot
// cover (per run_child_agent_test.go NOTE: ADK interrupt timing is not
// reproducible with stubs).
//
// Double-gated (AGENTS.md §5.3): skipped unless XIRA_DEEPSEEK_LIVE=1 AND
// DEEPSEEK_API_KEY are set. Run via `task live-test`.
//
// These tests CANNOT be run in CI (no GitHub CI configured) — run manually
// before release: `task live-test`.

// TestLiveSpawnTurnChildCompletes verifies the full spawn→poll lifecycle with
// a real LLM. The parent agent (xira-assistant) spawns a child
// (research-assistant), the child runs a real DeepSeek turn, and the parent
// polls for the result via poll_turn.
func TestLiveSpawnTurnChildCompletes(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)

	// Inject a SpawnBus so spawn_turn results have a home + poll_turn can
	// query. The Router does this in production; here we inject directly.
	collector := &liveTestSpawnCollector{}
	ctx := WithSpawnBus(context.Background(), collector)

	resp, err := rt.RunAgent(ctx, TurnRequest{
		Message: "Use spawn_turn to spawn the research-assistant with the task: 'In one sentence, explain what an LLM agent is.' Then use poll_turn to check if it finished. Report the child's result.",
		Context: channel.NewInboundContext("test", "live-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	t.Logf("live spawn response: status=%q final=%q", resp.Status, resp.FinalResponse)

	// The parent should have either spawned+polled (success) or at least
	// attempted spawn_turn. We can't force the LLM to call a specific tool,
	// but we CAN assert the agent didn't crash and produced output.
	if resp.FinalResponse == "" && resp.Status != StatusWaitingHuman {
		t.Errorf("live spawn: empty FinalResponse and status=%q — agent produced nothing", resp.Status)
	}

	// If the LLM did spawn, the collector should have received a result
	// (eventually — the child runs asynchronously). Wait briefly for the
	// detached goroutine to deliver.
	time.Sleep(2 * time.Second)
	if collector.count() > 0 {
		pr, ok := collector.latest()
		if ok {
			t.Logf("live spawn: child result received status=%q summary=%q", pr.Result.Status, pr.Result.Summary)
			if pr.Err != "" {
				t.Logf("live spawn: child error (non-fatal in smoke test): %s", pr.Err)
			}
		}
	} else {
		t.Log("live spawn: no child result in collector (LLM may not have called spawn_turn — acceptable for smoke test)")
	}
}

// TestLiveSpawnTurnNonBlocking verifies that spawn_turn does NOT block the
// ADK event loop (the PR #53 CRITICAL fix). If the parent calls spawn_turn
// and then continues reasoning (calls another tool or produces text), the
// turn completes normally — proving spawn is async (not a blocking wait).
func TestLiveSpawnTurnNonBlocking(t *testing.T) {
	rt := newLiveDeepSeekHITLService(t, false)

	collector := &liveTestSpawnCollector{}
	ctx := WithSpawnBus(context.Background(), collector)

	// Bound the turn — if spawn_turn blocked (regression), the turn would hang.
	done := make(chan struct{})
	var resp TurnResponse
	var runErr error
	go func() {
		resp, runErr = rt.RunAgent(ctx, TurnRequest{
			Message: "Use spawn_turn to spawn the research-assistant with the task: 'Say hello.' Immediately after calling spawn_turn, write 'Spawned.' — do NOT wait for or poll the child. You must respond immediately.",
			Context: channel.NewInboundContext("test", "live-user", nil),
		})
		close(done)
	}()

	select {
	case <-done:
		if runErr != nil {
			t.Fatalf("RunAgent() error = %v", runErr)
		}
		t.Logf("live spawn non-blocking: status=%q final=%q", resp.Status, resp.FinalResponse)
		// The agent should have responded (spawn_turn returned immediately,
		// the event loop kept iterating, the turn completed).
		if resp.Status == "" {
			t.Error("live spawn non-blocking: agent produced no response — may have blocked")
		}
	case <-time.After(60 * time.Second):
		t.Fatal("live spawn non-blocking: RunAgent hung for 60s — spawn_turn may be blocking the event loop (regression of PR #53 CRITICAL fix)")
	}
}

// --- test double ---

// liveTestSpawnCollector is a minimal SpawnBus for live tests. It records
// results so assertions can check delivery. Implements SpawnBus (Deliver) +
// SpawnBusPeeper (TryResult/HasResult).
type liveTestSpawnCollector struct {
	results []PendingResult
}

func (c *liveTestSpawnCollector) Deliver(pr PendingResult) {
	c.results = append(c.results, pr)
}

func (c *liveTestSpawnCollector) TryResult(childID string) (PendingResult, bool) {
	for i := range c.results {
		if c.results[i].TurnID == childID || strings.Contains(c.results[i].TurnID, childID) {
			return c.results[i], true
		}
	}
	return PendingResult{}, false
}

func (c *liveTestSpawnCollector) HasResult() bool {
	return len(c.results) > 0
}

func (c *liveTestSpawnCollector) count() int { return len(c.results) }
func (c *liveTestSpawnCollector) latest() (PendingResult, bool) {
	if len(c.results) == 0 {
		return PendingResult{}, false
	}
	return c.results[len(c.results)-1], true
}

// Compile-time: satisfies SpawnBus + SpawnBusPeeper.
var _ SpawnBus = (*liveTestSpawnCollector)(nil)
var _ SpawnBusPeeper = (*liveTestSpawnCollector)(nil)
