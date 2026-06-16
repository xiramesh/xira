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
			t.Fatalf("%s duplicate id %q", label, id)
		}
		seen[id] = struct{}{}
	}
}
