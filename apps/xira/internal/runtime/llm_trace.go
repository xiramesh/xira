package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ai-daming/xira/internal/model/deepseek"
)

const llmTraceEnv = "XIRA_TRACE_LLM"

func (s *Service) withLLMRequestTrace(
	ctx context.Context,
	runID string,
	recordEvent func(kind, source, message string, payload map[string]any),
) context.Context {
	if !llmTraceEnabled() || s == nil || s.runs == nil {
		return ctx
	}
	dir := filepath.Join(s.runs.RunDir(runID), "llm_requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		recordEvent("llm.trace_failed", "runtime", "failed to initialize llm request trace", map[string]any{"error": err.Error()})
		return ctx
	}
	var mu sync.Mutex
	var seq int
	recorder := func(_ context.Context, req deepseek.ChatRequest) {
		data, err := json.MarshalIndent(req, "", "  ")
		if err != nil {
			recordEvent("llm.trace_failed", "runtime", "failed to marshal llm request trace", map[string]any{"error": err.Error()})
			return
		}
		mu.Lock()
		defer mu.Unlock()
		seq++
		name := fmt.Sprintf("%03d.json", seq)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			recordEvent("llm.trace_failed", "runtime", "failed to write llm request trace", map[string]any{"path": path, "error": err.Error()})
			return
		}
		recordEvent("llm.request_traced", "deepseek", "llm request trace written", map[string]any{
			"path":     filepath.ToSlash(filepath.Join("llm_requests", name)),
			"model":    req.Model,
			"stream":   req.Stream,
			"messages": len(req.Messages),
			"tools":    len(req.Tools),
		})
	}
	return deepseek.WithRequestTraceRecorder(ctx, recorder)
}

func llmTraceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(llmTraceEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
