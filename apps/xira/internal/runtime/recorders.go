package runtime

import "sync"

type runRecorder struct {
	mu   sync.Mutex
	resp *TurnResponse
}

func newRunRecorder(resp *TurnResponse) *runRecorder {
	return &runRecorder{resp: resp}
}

func (r *runRecorder) appendEvent(evt RuntimeEvent) {
	if r == nil || r.resp == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resp.Events = append(r.resp.Events, evt)
}

func (r *runRecorder) appendAudit(evt AuditEvent) {
	if r == nil || r.resp == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resp.AuditEvents = append(r.resp.AuditEvents, evt)
}

func (r *runRecorder) appendLLMCall(call LLMCallRecord) {
	if r == nil || r.resp == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resp.LLMCalls = append(r.resp.LLMCalls, call)
}

type toolCallRecorder struct {
	mu      sync.Mutex
	records []ToolCallRecord
}

func (r *toolCallRecorder) append(rec ToolCallRecord) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
}

func (r *toolCallRecorder) snapshot() []ToolCallRecord {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ToolCallRecord(nil), r.records...)
}
