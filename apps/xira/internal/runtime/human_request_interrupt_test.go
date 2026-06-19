package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/humanrequest"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

func TestHumanRequestToolIsAvailableToNativeProfiles(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	defs := rt.toolDefinitions(context.Background(), agents.BuiltinXiraAssistant())
	for _, def := range defs {
		if def.Function.Name == "human_request" {
			return
		}
	}
	t.Fatalf("native tool definitions missing human_request: %+v", defs)
}

func TestHumanRequestToolCanBeDisabledForApprovedToolReplay(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	ctx := contextWithRuntimeNativeToolsDisabled(context.Background())
	defs := rt.toolDefinitions(ctx, agents.BuiltinXiraAssistant())
	for _, def := range defs {
		if def.Function.Name == "human_request" {
			t.Fatalf("native tool definitions exposed human_request in replay-disabled context: %+v", defs)
		}
	}
	adkTools, err := rt.adkTools(ctx, agents.BuiltinXiraAssistant(), func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range adkTools {
		if tool.Name() == "human.request" {
			t.Fatalf("ADK tools exposed human.request in replay-disabled context: %+v", adkToolNames(adkTools))
		}
	}
}

func TestHumanRequestToolIsAvailableToADKProfiles(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	tools, err := rt.adkTools(context.Background(), agents.BuiltinXiraAssistant(), func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {}, func(ToolCallRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	if !adkToolNames(tools)["human.request"] {
		t.Fatalf("ADK tools missing human.request: %+v", adkToolNames(tools))
	}
	if adkToolNames(tools)["human.respond"] {
		t.Fatalf("ADK tools should not expose human.respond to model calls: %+v", adkToolNames(tools))
	}
}

func TestHumanRequestToolCreatesPendingRequestAndInterrupt(t *testing.T) {
	var modelCalls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		modelCalls++
		if modelCalls > 1 {
			t.Fatalf("model called after human.request interrupt")
		}
		var req deepseek.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("human-call-1", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Which deployment window should I use?",
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "ask a human", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != "waiting_human" {
		t.Fatalf("status = %q", resp.Status)
	}
	if resp.Interrupt == nil || resp.Interrupt.Status != "waiting_human" {
		t.Fatalf("interrupt = %+v", resp.Interrupt)
	}
	if len(resp.HumanRequests) != 1 {
		t.Fatalf("human_requests = %+v", resp.HumanRequests)
	}
	request := resp.HumanRequests[0]
	if request.Status != humanrequest.StatusPending || request.Question != "Which deployment window should I use?" {
		t.Fatalf("human request = %+v", request)
	}
	if len(resp.Interrupt.BlockedBy) != 1 || resp.Interrupt.BlockedBy[0].Type != "human_request" || resp.Interrupt.BlockedBy[0].HumanRequestID != request.ID {
		t.Fatalf("blocked_by = %+v", resp.Interrupt.BlockedBy)
	}
	if len(resp.ToolCalls) != 0 {
		t.Fatalf("human.request should not be persisted as ordinary tool transcript: %+v", resp.ToolCalls)
	}
	if modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", modelCalls)
	}
	stored, err := rt.GetHumanRequest(context.Background(), request.ID)
	if err != nil {
		t.Fatalf("stored human request: %v", err)
	}
	if stored.ID != request.ID || stored.Status != humanrequest.StatusPending {
		t.Fatalf("stored human request = %+v", stored)
	}
}

func TestHumanRequestToolDefaultsMissingKindToFreeform(t *testing.T) {
	var modelCalls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		if modelCalls > 1 {
			t.Fatalf("model called after human.request interrupt")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("human-call-missing-kind", "human_request", map[string]any{
				"question": "Which deployment window should I use?",
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "ask a human without explicit kind", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != StatusWaitingHuman || len(resp.HumanRequests) != 1 {
		t.Fatalf("human_requests = %+v status=%q final=%q", resp.HumanRequests, resp.Status, resp.FinalResponse)
	}
	if resp.HumanRequests[0].Kind != humanrequest.RequestFreeform {
		t.Fatalf("missing kind should default to freeform: %+v", resp.HumanRequests[0])
	}
}

func TestRunInterruptSetsWaitingHumanStatus(t *testing.T) {
	rt, resp, _ := runHumanRequestInterrupt(t, "human-status-call", "Need status confirmation?")
	if resp.Status != StatusWaitingHuman || resp.Interrupt == nil || resp.Interrupt.Status != StatusWaitingHuman {
		t.Fatalf("response status=%q interrupt=%+v", resp.Status, resp.Interrupt)
	}
	stored, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusWaitingHuman {
		t.Fatalf("stored run status = %q, want %q", stored.Status, StatusWaitingHuman)
	}
}

func TestRunInterruptIncludesHumanRequestsAndBlockedBy(t *testing.T) {
	_, resp, _ := runHumanRequestInterrupt(t, "human-blocked-call", "Need blocked_by confirmation?")
	if len(resp.HumanRequests) != 1 {
		t.Fatalf("human_requests = %+v", resp.HumanRequests)
	}
	if resp.Interrupt == nil || len(resp.Interrupt.BlockedBy) != 1 {
		t.Fatalf("interrupt = %+v", resp.Interrupt)
	}
	blocked := resp.Interrupt.BlockedBy[0]
	if blocked.Type != "human_request" || blocked.HumanRequestID != resp.HumanRequests[0].ID {
		t.Fatalf("blocked_by = %+v, request=%+v", resp.Interrupt.BlockedBy, resp.HumanRequests[0])
	}
}

func TestRunInterruptDoesNotValidateFinalResponse(t *testing.T) {
	_, resp, _ := runHumanRequestInterrupt(t, "human-no-final-call", "Need missing-final confirmation?")
	if strings.TrimSpace(resp.FinalResponse) != "" {
		t.Fatalf("final response = %q, want empty suspended final", resp.FinalResponse)
	}
	if resp.Status != StatusWaitingHuman || resp.VerificationResult.Status != StatusWaitingHuman {
		t.Fatalf("status=%q verification=%+v", resp.Status, resp.VerificationResult)
	}
	if resp.EvolutionCandidate != nil {
		t.Fatalf("waiting_human created evolution candidate: %+v", resp.EvolutionCandidate)
	}
}

func TestRunInterruptPersistsUsageAndEvents(t *testing.T) {
	rt, resp, modelCalls := runHumanRequestInterrupt(t, "human-events-call", "Need event confirmation?")
	if modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", modelCalls)
	}
	if resp.Usage.CallCount == 0 {
		t.Fatalf("usage = %+v, want recorded model call", resp.Usage)
	}
	if _, ok := findEvent(resp.Events, "run.waiting_human"); !ok {
		t.Fatalf("events missing run.waiting_human: %+v", eventKinds(resp.Events))
	}
	stored, err := rt.RunStore().Load(resp.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Usage.CallCount == 0 || len(stored.Events) == 0 {
		t.Fatalf("stored usage/events = %+v / %+v", stored.Usage, eventKinds(stored.Events))
	}
}

func TestNativePathStopsBeforeSecondModelCallOnInterrupt(t *testing.T) {
	var modelCalls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		modelCalls++
		if modelCalls > 1 {
			t.Fatalf("native model called after human.request interrupt")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("native-human-call", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Need native approval?",
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	runID := "native-human-run"
	if err := rt.RunStore().InitRun(runID); err != nil {
		t.Fatal(err)
	}
	profile := agents.BuiltinXiraAssistant()
	collector := newRuntimeSuspendCollector()
	ctx := contextWithRuntimeSuspendCollector(context.Background(), collector)
	ctx = contextWithToolTrace(ctx, runID)
	ctx = contextWithRunExecution(ctx, runExecutionContext{
		Base: runtimeEventBase{
			RunID:                 runID,
			AgentID:               profile.ID,
			ConversationSessionID: "session-native",
			TraceID:               runID,
		},
		Profile: profile,
		Request: TurnRequest{Message: "ask native", SessionID: "session-native"},
	})
	ctx = rt.withLLMInstrumentation(ctx, llmInstrumentationInput{
		RunID:   runID,
		AgentID: profile.ID,
		Pricing: rt.pricing,
	}, func(string, string, string, map[string]any) {}, func(LLMCallRecord) {})

	final, toolCalls, err := rt.generateNativeDeepSeek(ctx, profile, "native instruction", TurnRequest{
		Message:   "ask native",
		SessionID: "session-native",
	}, func(string, string, string, map[string]any) {}, func(string, string, bool, string, map[string]any) {})
	if err != nil {
		t.Fatalf("generateNativeDeepSeek() error = %v", err)
	}
	if strings.TrimSpace(final) != "" {
		t.Fatalf("final = %q, want empty suspended final", final)
	}
	if len(toolCalls) != 0 {
		t.Fatalf("human.request should not be ordinary native tool call: %+v", toolCalls)
	}
	if !collector.HasInterrupt() || len(collector.Interrupt().HumanRequests) != 1 {
		t.Fatalf("collector interrupt = %+v", collector.Interrupt())
	}
	if modelCalls != 1 {
		t.Fatalf("model calls = %d, want 1", modelCalls)
	}
}

func TestADKPathStopsBeforeSecondModelCallOnInterrupt(t *testing.T) {
	var modelCalls int
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		modelCalls++
		if modelCalls > 1 {
			t.Fatalf("ADK model called after human.request interrupt")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("adk-human-call", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Need ADK approval?",
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "ask via adk", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != StatusWaitingHuman || resp.Interrupt == nil || len(resp.HumanRequests) != 1 {
		t.Fatalf("ADK waiting response = %+v", resp)
	}
	if modelCalls != 1 {
		t.Fatalf("ADK model calls = %d, want 1", modelCalls)
	}
}

func TestRunInterruptDoesNotValidateFinalResponseOrCreateEvolutionCandidate(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("human-call-2", "human_request", map[string]any{
				"kind":     "freeform",
				"question": "Need missing final response?",
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "ask human", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != "waiting_human" || resp.VerificationResult.Status != "waiting_human" {
		t.Fatalf("status=%q verification=%+v", resp.Status, resp.VerificationResult)
	}
	if resp.EvolutionCandidate != nil {
		t.Fatalf("waiting_human created evolution candidate: %+v", resp.EvolutionCandidate)
	}
	if resp.EndedAt.IsZero() || resp.Usage.CallCount == 0 {
		t.Fatalf("waiting_human did not close turn with usage: ended=%v usage=%+v", resp.EndedAt, resp.Usage)
	}
	if _, ok := findEvent(resp.Events, "run.waiting_human"); !ok {
		t.Fatalf("events missing run.waiting_human: %+v", eventKinds(resp.Events))
	}
}

func runHumanRequestInterrupt(t *testing.T, callID, question string) (*Service, TurnResponse, int) {
	t.Helper()
	var modelCalls int
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		modelCalls++
		if modelCalls > 1 {
			t.Fatalf("model called after human.request interrupt")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs(callID, "human_request", map[string]any{
				"kind":     "freeform",
				"question": question,
			}))),
		}, nil
	})}
	rt := newTestService(t, Config{
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "ask a human", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	return rt, resp, modelCalls
}

func TestHumanRequestToolDedupesRepeatedSameToolCall(t *testing.T) {
	rt, ctx := newHumanRequestToolTestRuntime(t, "run-dedupe", "session-dedupe")

	first, err := rt.createAgentHumanRequest(ctx, "human-call-same", map[string]any{
		"kind":     "freeform",
		"question": "Same question?",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := rt.createAgentHumanRequest(ctx, "human-call-same", map[string]any{
		"kind":     "freeform",
		"question": "Same question?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate request id = %q, want %q", second.ID, first.ID)
	}
	list, err := rt.humanRequests.List(context.Background(), humanrequest.ListQuery{WorkspaceKey: rt.WorkspaceKey(), Status: humanrequest.StatusPending})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("pending requests = %+v, want one deduped request", list)
	}
}

func TestHumanRequestToolAllowsMultipleDistinctQuestions(t *testing.T) {
	rt, ctx := newHumanRequestToolTestRuntime(t, "run-distinct", "session-distinct")
	if _, err := rt.createAgentHumanRequest(ctx, "human-call-a", map[string]any{"kind": "freeform", "question": "Question A?"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.createAgentHumanRequest(ctx, "human-call-b", map[string]any{"kind": "freeform", "question": "Question B?"}); err != nil {
		t.Fatal(err)
	}
	list, err := rt.humanRequests.List(context.Background(), humanrequest.ListQuery{WorkspaceKey: rt.WorkspaceKey(), Status: humanrequest.StatusPending})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("pending requests = %+v, want two distinct requests", list)
	}
}

func TestHumanRequestToolRejectsInvalidOptions(t *testing.T) {
	rt, ctx := newHumanRequestToolTestRuntime(t, "run-duplicate-options", "session-duplicate-options")

	_, err := rt.createAgentHumanRequest(ctx, "human-call-options", map[string]any{
		"kind":     "approval",
		"question": "Pick a deployment window.",
		"options": []any{
			map[string]any{"id": "window-a", "label": "Tuesday"},
			map[string]any{"id": "window-a", "label": "Wednesday"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate option id") {
		t.Fatalf("createAgentHumanRequest duplicate options error = %v", err)
	}
	list, err := rt.humanRequests.List(context.Background(), humanrequest.ListQuery{WorkspaceKey: rt.WorkspaceKey(), Status: humanrequest.StatusPending})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("duplicate option request should not be persisted: %+v", list)
	}
}

func newHumanRequestToolTestRuntime(t *testing.T, runID, sessionID string) (*Service, context.Context) {
	t.Helper()
	rt := newTestService(t, Config{StateDir: filepath.Join(t.TempDir(), "state")})
	if err := rt.RunStore().InitRun(runID); err != nil {
		t.Fatal(err)
	}
	profile := agents.BuiltinXiraAssistant()
	collector := newRuntimeSuspendCollector()
	ctx := contextWithRuntimeSuspendCollector(context.Background(), collector)
	ctx = contextWithRunExecution(ctx, runExecutionContext{
		Base: runtimeEventBase{
			RunID:                 runID,
			AgentID:               profile.ID,
			ConversationSessionID: sessionID,
			TraceID:               runID,
		},
		Profile: profile,
		Request: TurnRequest{Message: "ask", SessionID: sessionID},
	})
	return rt, ctx
}
