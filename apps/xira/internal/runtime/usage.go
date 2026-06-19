package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

type UsagePricing struct {
	Currency string                       `json:"currency,omitempty" yaml:"currency,omitempty"`
	Models   map[string]ModelUsagePricing `json:"models,omitempty" yaml:"models,omitempty"`
}

type ModelUsagePricing struct {
	PromptPerMillion     float64 `json:"prompt_per_million,omitempty" yaml:"prompt_per_million,omitempty"`
	CompletionPerMillion float64 `json:"completion_per_million,omitempty" yaml:"completion_per_million,omitempty"`
}

type UsageStore struct {
	stateDir string
	mu       sync.Mutex
}

func NewUsageStore(stateDir string) *UsageStore {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = ".xira"
	}
	return &UsageStore{stateDir: stateDir}
}

func (s *UsageStore) Root() string {
	if s == nil {
		return ""
	}
	return s.stateDir
}

func (s *UsageStore) AppendCalls(calls []LLMCallRecord) error {
	if s == nil || len(calls) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.stateDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(s.stateDir, "usage-ledger.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, call := range calls {
		data, err := json.Marshal(call)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return err
		}
	}
	return nil
}

type llmInstrumentationInput struct {
	RunID          string
	AgentID        string
	EntrypointID   string
	Channel        string
	SessionID      string
	AgentSessionID string
	ADKSessionID   string
	UserID         string
	Pricing        UsagePricing
}

func (s *Service) withLLMInstrumentation(
	ctx context.Context,
	input llmInstrumentationInput,
	recordEvent func(kind, source, message string, payload map[string]any),
	appendCall func(LLMCallRecord),
) context.Context {
	recorder := &llmCallRecorder{
		service:     s,
		input:       input,
		recordEvent: recordEvent,
		appendCall:  appendCall,
		tracePaths:  []string{},
	}
	ctx = deepseek.WithCallTraceRecorder(ctx, recorder.recordCall)
	ctx = deepseek.WithRequestTraceRecorder(ctx, recorder.recordRequestTrace)
	ctx = deepseek.WithRawTraceRecorder(ctx, recorder.recordRawTrace)
	return ctx
}

type llmCallRecorder struct {
	service     *Service
	input       llmInstrumentationInput
	recordEvent func(kind, source, message string, payload map[string]any)
	appendCall  func(LLMCallRecord)

	mu         sync.Mutex
	callSeq    int
	traceSeq   int
	rawSeq     int
	activeRaw  int
	tracePaths []string
	rawPaths   []string
}

func (r *llmCallRecorder) recordRequestTrace(_ context.Context, req deepseek.ChatRequest) {
	if !llmTraceEnabled() || r.service == nil || r.service.runs == nil {
		return
	}
	dir := filepath.Join(r.service.runs.RunDir(r.input.RunID), "llm_requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.recordEvent("llm.trace_failed", "runtime", "failed to initialize llm request trace", map[string]any{"error": err.Error()})
		return
	}
	data, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		r.recordEvent("llm.trace_failed", "runtime", "failed to marshal llm request trace", map[string]any{"error": err.Error()})
		return
	}

	r.mu.Lock()
	r.traceSeq++
	name := fmt.Sprintf("%03d.json", r.traceSeq)
	relPath := filepath.ToSlash(filepath.Join("llm_requests", name))
	r.tracePaths = append(r.tracePaths, relPath)
	r.mu.Unlock()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		r.recordEvent("llm.trace_failed", "runtime", "failed to write llm request trace", map[string]any{"path": path, "error": err.Error()})
		return
	}
	r.recordEvent("llm.request_traced", "deepseek", "llm request trace written", map[string]any{
		"path":     relPath,
		"model":    req.Model,
		"stream":   req.Stream,
		"messages": len(req.Messages),
		"tools":    len(req.Tools),
	})
}

func (r *llmCallRecorder) recordCall(_ context.Context, trace deepseek.CallTrace) {
	r.mu.Lock()
	r.callSeq++
	requestIndex := r.callSeq
	tracePath := ""
	if len(r.tracePaths) > 0 {
		tracePath = r.tracePaths[0]
		r.tracePaths = r.tracePaths[1:]
	}
	rawTracePath := ""
	if len(r.rawPaths) > 0 {
		rawTracePath = r.rawPaths[0]
		r.rawPaths = r.rawPaths[1:]
	}
	r.mu.Unlock()

	call := buildLLMCallRecord(r.input, requestIndex, tracePath, trace)
	call.RawTracePath = rawTracePath
	if call.UsageSource == "provider" {
		cost, currency := usageCost(r.input.Pricing, call.Model, call.PromptTokens, call.CompletionTokens)
		call.Cost = cost
		call.Currency = currency
	}
	if r.appendCall != nil {
		r.appendCall(call)
	}
	payload := map[string]any{
		"request_index":     call.RequestIndex,
		"model":             call.Model,
		"status":            call.Status,
		"usage_source":      call.UsageSource,
		"prompt_tokens":     call.PromptTokens,
		"completion_tokens": call.CompletionTokens,
		"total_tokens":      call.TotalTokens,
		"latency_ms":        call.LatencyMS,
	}
	if call.Cost != nil {
		payload["cost"] = *call.Cost
		payload["currency"] = call.Currency
	}
	r.recordEvent("llm.call_recorded", "deepseek", "llm call usage recorded", payload)
}

func (r *llmCallRecorder) recordRawTrace(_ context.Context, trace deepseek.RawTrace) {
	if !llmTraceEnabled() || r.service == nil || r.service.runs == nil {
		return
	}

	switch trace.Event {
	case "request_body":
		seq, relDir := r.nextRawTraceDir()
		dir := filepath.Join(r.service.runs.RunDir(r.input.RunID), relDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			r.recordEvent("llm.raw_trace_failed", "runtime", "failed to initialize raw llm trace", map[string]any{"error": err.Error()})
			return
		}
		if err := os.WriteFile(filepath.Join(dir, "request.body"), trace.Body, 0o644); err != nil {
			r.recordEvent("llm.raw_trace_failed", "runtime", "failed to write raw llm request", map[string]any{"error": err.Error(), "path": filepath.ToSlash(filepath.Join(relDir, "request.body"))})
			return
		}
		meta := map[string]any{
			"event":       trace.Event,
			"method":      trace.Method,
			"url":         trace.URL,
			"bytes":       len(trace.Body),
			"recorded_at": time.Now(),
		}
		if err := writeRawTraceJSON(filepath.Join(dir, "request.meta.json"), meta); err != nil {
			r.recordEvent("llm.raw_trace_failed", "runtime", "failed to write raw llm request metadata", map[string]any{"error": err.Error(), "path": filepath.ToSlash(filepath.Join(relDir, "request.meta.json"))})
			return
		}
		r.recordEvent("llm.raw_request_traced", "deepseek", "raw llm request written", map[string]any{
			"index": seq,
			"path":  filepath.ToSlash(filepath.Join(relDir, "request.body")),
			"bytes": len(trace.Body),
		})
	case "response_status":
		seq, relDir := r.currentRawTraceDir()
		if seq == 0 {
			return
		}
		dir := filepath.Join(r.service.runs.RunDir(r.input.RunID), relDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			r.recordEvent("llm.raw_trace_failed", "runtime", "failed to initialize raw llm trace", map[string]any{"error": err.Error()})
			return
		}
		meta := map[string]any{
			"event":       trace.Event,
			"status_code": trace.StatusCode,
			"headers":     trace.Header,
			"recorded_at": time.Now(),
		}
		if err := writeRawTraceJSON(filepath.Join(dir, "response.meta.json"), meta); err != nil {
			r.recordEvent("llm.raw_trace_failed", "runtime", "failed to write raw llm response metadata", map[string]any{"error": err.Error(), "path": filepath.ToSlash(filepath.Join(relDir, "response.meta.json"))})
			return
		}
		r.recordEvent("llm.raw_response_status_traced", "deepseek", "raw llm response status written", map[string]any{
			"index":       seq,
			"path":        filepath.ToSlash(filepath.Join(relDir, "response.meta.json")),
			"status_code": trace.StatusCode,
		})
	case "response_body", "response_chunk":
		_, relDir := r.currentRawTraceDir()
		if relDir == "" {
			return
		}
		dir := filepath.Join(r.service.runs.RunDir(r.input.RunID), relDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			r.recordEvent("llm.raw_trace_failed", "runtime", "failed to initialize raw llm trace", map[string]any{"error": err.Error()})
			return
		}
		if err := appendRawTraceBytes(filepath.Join(dir, "response.body"), trace.Body); err != nil {
			r.recordEvent("llm.raw_trace_failed", "runtime", "failed to write raw llm response", map[string]any{"error": err.Error(), "path": filepath.ToSlash(filepath.Join(relDir, "response.body"))})
		}
	case "response_error":
		_, relDir := r.currentRawTraceDir()
		if relDir == "" {
			return
		}
		dir := filepath.Join(r.service.runs.RunDir(r.input.RunID), relDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			r.recordEvent("llm.raw_trace_failed", "runtime", "failed to initialize raw llm trace", map[string]any{"error": err.Error()})
			return
		}
		if err := os.WriteFile(filepath.Join(dir, "response.error.txt"), []byte(trace.Error+"\n"), 0o644); err != nil {
			r.recordEvent("llm.raw_trace_failed", "runtime", "failed to write raw llm response error", map[string]any{"error": err.Error(), "path": filepath.ToSlash(filepath.Join(relDir, "response.error.txt"))})
		}
	}
}

func (r *llmCallRecorder) nextRawTraceDir() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rawSeq++
	r.activeRaw = r.rawSeq
	relDir := filepath.ToSlash(filepath.Join("llm_raw", fmt.Sprintf("%03d", r.rawSeq)))
	r.rawPaths = append(r.rawPaths, relDir)
	return r.rawSeq, relDir
}

func (r *llmCallRecorder) currentRawTraceDir() (int, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeRaw == 0 {
		return 0, ""
	}
	return r.activeRaw, filepath.ToSlash(filepath.Join("llm_raw", fmt.Sprintf("%03d", r.activeRaw)))
}

func writeRawTraceJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func appendRawTraceBytes(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func buildLLMCallRecord(input llmInstrumentationInput, requestIndex int, tracePath string, trace deepseek.CallTrace) LLMCallRecord {
	endedAt := trace.EndedAt
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	startedAt := trace.StartedAt
	if startedAt.IsZero() {
		startedAt = endedAt
	}
	status := "completed"
	errText := ""
	if trace.Err != nil {
		status = "failed"
		errText = trace.Err.Error()
	}
	model := strings.TrimSpace(trace.Request.Model)
	if trace.Response != nil && strings.TrimSpace(trace.Response.Model) != "" {
		model = strings.TrimSpace(trace.Response.Model)
	}
	usage := map[string]any(nil)
	if trace.Response != nil && len(trace.Response.Usage) > 0 {
		usage = copyUsageMap(trace.Response.Usage)
	}
	promptTokens, completionTokens, totalTokens, usageSource := usageTokens(usage)
	promptChars, toolResultChars := requestCharStats(trace.Request)
	thinkingType := ""
	if trace.Request.Thinking != nil {
		thinkingType = trace.Request.Thinking.Type
	}
	return LLMCallRecord{
		RunID:            input.RunID,
		AgentID:          input.AgentID,
		EntrypointID:     input.EntrypointID,
		Channel:          input.Channel,
		SessionID:        input.SessionID,
		AgentSessionID:   input.AgentSessionID,
		ADKSessionID:     input.ADKSessionID,
		UserID:           input.UserID,
		Provider:         "deepseek",
		Model:            model,
		RequestIndex:     requestIndex,
		Status:           status,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		LatencyMS:        endedAt.Sub(startedAt).Milliseconds(),
		Stream:           trace.Request.Stream,
		Temperature:      cloneFloat32(trace.Request.Temperature),
		ThinkingType:     thinkingType,
		MessageCount:     len(trace.Request.Messages),
		ToolCount:        len(trace.Request.Tools),
		PromptChars:      promptChars,
		ToolResultChars:  toolResultChars,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		UsageSource:      usageSource,
		Error:            errText,
		TraceRequestPath: tracePath,
		ProviderUsage:    usage,
	}
}

func summarizeUsage(resp TurnResponse) UsageSummary {
	summary := UsageSummary{
		RunID:        resp.RunID,
		AgentID:      resp.AgentID,
		EntrypointID: resp.EntrypointID,
		SessionID:    resp.SessionID,
		StartedAt:    resp.StartedAt,
		EndedAt:      resp.EndedAt,
		UsageSources: map[string]int{},
		Models:       map[string]UsageModelSummary{},
	}
	if resp.SessionScope != nil {
		summary.Channel = resp.SessionScope.Channel
	}
	var totalCost float64
	var hasCost bool
	for _, call := range resp.LLMCalls {
		summary.CallCount++
		if call.Status == "failed" {
			summary.FailedCalls++
		} else {
			summary.CompletedCalls++
		}
		summary.PromptTokens += call.PromptTokens
		summary.CompletionTokens += call.CompletionTokens
		summary.TotalTokens += call.TotalTokens
		summary.UsageSources[call.UsageSource]++
		if call.UsageSource != "provider" {
			summary.MissingUsageRequests = append(summary.MissingUsageRequests, call.RequestIndex)
		}
		if call.Cost != nil {
			totalCost += *call.Cost
			hasCost = true
			if summary.Currency == "" {
				summary.Currency = call.Currency
			}
		}
		modelSummary := summary.Models[call.Model]
		modelSummary.CallCount++
		modelSummary.PromptTokens += call.PromptTokens
		modelSummary.CompletionTokens += call.CompletionTokens
		modelSummary.TotalTokens += call.TotalTokens
		if call.Cost != nil {
			current := float64(0)
			if modelSummary.Cost != nil {
				current = *modelSummary.Cost
			}
			current += *call.Cost
			modelSummary.Cost = &current
			modelSummary.Currency = call.Currency
		}
		summary.Models[call.Model] = modelSummary
	}
	if hasCost {
		summary.Cost = &totalCost
	}
	if len(summary.UsageSources) == 0 {
		summary.UsageSources = nil
	}
	if len(summary.Models) == 0 {
		summary.Models = nil
	}
	return summary
}

func modelPolicySnapshot(profile agents.Profile, profileSource string) ModelPolicySnapshot {
	return ModelPolicySnapshot{
		AgentID:         profile.ID,
		Provider:        profile.ModelPolicy.Provider,
		Model:           profile.ModelPolicy.Model,
		Stream:          profile.ModelPolicy.Stream,
		Temperature:     cloneFloat32(profile.ModelPolicy.Temp),
		ThinkingType:    thinkingType(profile.ModelPolicy),
		Tools:           append([]string{}, profile.Permissions.Tools...),
		Skills:          append([]string{}, profile.Skills...),
		ProfileSource:   profileSource,
		InstructionHash: instructionHash(profile.InstructionText()),
	}
}

func normalizeUsagePricing(pricing UsagePricing) UsagePricing {
	pricing.Currency = strings.TrimSpace(pricing.Currency)
	if len(pricing.Models) == 0 {
		pricing.Models = nil
		return pricing
	}
	models := map[string]ModelUsagePricing{}
	for model, value := range pricing.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		models[model] = value
	}
	if len(models) == 0 {
		models = nil
	}
	pricing.Models = models
	return pricing
}

func usageCost(pricing UsagePricing, model string, promptTokens, completionTokens int64) (*float64, string) {
	if len(pricing.Models) == 0 {
		return nil, ""
	}
	modelPricing, ok := pricing.Models[model]
	if !ok || (modelPricing.PromptPerMillion == 0 && modelPricing.CompletionPerMillion == 0) {
		return nil, ""
	}
	cost := (float64(promptTokens)/1_000_000)*modelPricing.PromptPerMillion +
		(float64(completionTokens)/1_000_000)*modelPricing.CompletionPerMillion
	return &cost, strings.TrimSpace(pricing.Currency)
}

func usageTokens(usage map[string]any) (int64, int64, int64, string) {
	if len(usage) == 0 {
		return 0, 0, 0, "missing"
	}
	prompt := usageInt(usage, "prompt_tokens", "prompt_token_count", "input_tokens")
	completion := usageInt(usage, "completion_tokens", "completion_token_count", "candidates_token_count", "output_tokens")
	total := usageInt(usage, "total_tokens", "total_token_count")
	if total == 0 && prompt+completion > 0 {
		total = prompt + completion
	}
	return prompt, completion, total, "provider"
}

func usageInt(usage map[string]any, keys ...string) int64 {
	for _, key := range keys {
		value, ok := usage[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case int:
			return int64(typed)
		case int64:
			return typed
		case float64:
			if !math.IsNaN(typed) && !math.IsInf(typed, 0) {
				return int64(typed)
			}
		case json.Number:
			if got, err := typed.Int64(); err == nil {
				return got
			}
		case string:
			var parsed json.Number = json.Number(strings.TrimSpace(typed))
			if got, err := parsed.Int64(); err == nil {
				return got
			}
		}
	}
	return 0
}

func requestCharStats(req deepseek.ChatRequest) (int, int) {
	var promptChars int
	var toolResultChars int
	for _, msg := range req.Messages {
		chars := anyChars(msg.Content)
		promptChars += chars
		if msg.Role == "tool" {
			toolResultChars += chars
		}
	}
	for _, tool := range req.Tools {
		promptChars += utf8.RuneCountInString(tool.Function.Name)
		promptChars += utf8.RuneCountInString(tool.Function.Description)
		if len(tool.Function.Parameters) > 0 {
			data, _ := json.Marshal(tool.Function.Parameters)
			promptChars += utf8.RuneCount(data)
		}
	}
	return promptChars, toolResultChars
}

func anyChars(value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case string:
		return utf8.RuneCountInString(typed)
	case []byte:
		return utf8.RuneCount(typed)
	default:
		data, _ := json.Marshal(typed)
		return utf8.RuneCount(data)
	}
}

func copyUsageMap(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		out[key] = value
	}
	return out
}

func cloneFloat32(value *float32) *float32 {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}

func thinkingType(policy agents.ModelPolicy) string {
	if strings.TrimSpace(policy.Thinking.Type) != "" {
		return strings.TrimSpace(policy.Thinking.Type)
	}
	return "disabled"
}

func instructionHash(instruction string) string {
	sum := sha256.Sum256([]byte(instruction))
	return hex.EncodeToString(sum[:])[:16]
}

func filepathJoinSlash(elem ...string) string {
	return filepath.ToSlash(filepath.Join(elem...))
}
