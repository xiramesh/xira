package runtime

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRuntimeRecordersPreserveConcurrentAppends(t *testing.T) {
	const total = 1000
	resp := TurnResponse{RunID: "run-concurrent-recorders"}
	runRec := newRunRecorder(&resp)
	toolRec := &toolCallRecorder{}

	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		i := i
		wg.Add(4)
		go func() {
			defer wg.Done()
			runRec.appendEvent(RuntimeEvent{ID: fmt.Sprintf("event-%04d", i), RunID: resp.RunID, Kind: "tool.started"})
		}()
		go func() {
			defer wg.Done()
			runRec.appendAudit(AuditEvent{ID: fmt.Sprintf("audit-%04d", i), RunID: resp.RunID, Time: time.Unix(0, int64(i)), Action: "tool.call", Target: "read_file", Allowed: true})
		}()
		go func() {
			defer wg.Done()
			runRec.appendLLMCall(LLMCallRecord{RunID: resp.RunID, Provider: "deepseek", Model: "deepseek-test", StartedAt: time.Unix(0, int64(i))})
		}()
		go func() {
			defer wg.Done()
			toolRec.append(ToolCallRecord{ID: fmt.Sprintf("tool-%04d", i), RunID: resp.RunID, Name: "read_file", Input: map[string]any{"path": fmt.Sprintf("artifact-%04d.md", i)}})
		}()
	}
	wg.Wait()

	if len(resp.Events) != total {
		t.Fatalf("events = %d, want %d", len(resp.Events), total)
	}
	if len(resp.AuditEvents) != total {
		t.Fatalf("audit_events = %d, want %d", len(resp.AuditEvents), total)
	}
	if len(resp.LLMCalls) != total {
		t.Fatalf("llm_calls = %d, want %d", len(resp.LLMCalls), total)
	}
	records := toolRec.snapshot()
	if len(records) != total {
		t.Fatalf("tool_records = %d, want %d", len(records), total)
	}

	assertUniqueRecorderIDs(t, "events", total, func(i int) string { return resp.Events[i].ID })
	assertUniqueRecorderIDs(t, "audit_events", total, func(i int) string { return resp.AuditEvents[i].ID })
	assertUniqueRecorderIDs(t, "tool_records", total, func(i int) string { return records[i].ID })
}

func assertUniqueRecorderIDs(t *testing.T, label string, total int, idAt func(int) string) {
	t.Helper()
	seen := make(map[string]struct{}, total)
	for i := 0; i < total; i++ {
		id := idAt(i)
		if id == "" {
			t.Fatalf("%s[%d] missing id", label, i)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("%s duplicate id %q", label, i)
		}
		seen[id] = struct{}{}
	}
}

// TestRuntimeRecordersNilGuards: every append method is safe on a nil receiver
// and on a recorder with a nil resp (covers the early-return arms left at 80%).
func TestRuntimeRecordersNilGuards(t *testing.T) {
	var nilRec *runRecorder
	nilRec.appendEvent(RuntimeEvent{Kind: "x"})      // must not panic
	nilRec.appendAudit(AuditEvent{Action: "x"})
	nilRec.appendLLMCall(LLMCallRecord{Model: "m"})

	emptyRec := &runRecorder{resp: nil} // recorder exists, no response
	emptyRec.appendEvent(RuntimeEvent{Kind: "x"})
	emptyRec.appendAudit(AuditEvent{Action: "x"})
	emptyRec.appendLLMCall(LLMCallRecord{Model: "m"})

	var nilTool *toolCallRecorder
	nilTool.append(ToolCallRecord{Name: "x"})
	if got := nilTool.snapshot(); got != nil {
		t.Fatalf("nil tool recorder snapshot should be nil, got %v", got)
	}
}

// TestToolCallRecorderSnapshotIsCopy: snapshot returns an independent copy.
func TestToolCallRecorderSnapshotIsCopy(t *testing.T) {
	rec := &toolCallRecorder{}
	rec.append(ToolCallRecord{Name: "a"})
	rec.append(ToolCallRecord{Name: "b"})
	snap := rec.snapshot()
	if len(snap) != 2 || snap[1].Name != "b" {
		t.Fatalf("snapshot content wrong: %v", snap)
	}
	snap[0].Name = "mutated"
	if again := rec.snapshot(); again[0].Name != "a" {
		t.Fatalf("snapshot was not a copy: %v", again)
	}
}
