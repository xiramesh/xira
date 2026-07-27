package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

type reasoningLookupArgs struct {
	Query string `json:"query"`
}

func TestADKRunnerPreservesReasoningContentAcrossConsecutiveToolTurns(t *testing.T) {
	var mu sync.Mutex
	var requests []ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, req)
		requestNumber := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch requestNumber {
		case 1:
			_, _ = fmt.Fprint(w, `{"model":"deepseek-v4-flash","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","reasoning_content":"reason-one","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"alpha\"}"}}]}}]}`)
		case 2:
			_, _ = fmt.Fprint(w, `{"model":"deepseek-v4-flash","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","reasoning_content":"reason-two","tool_calls":[{"id":"call-2","type":"function","function":{"name":"lookup","arguments":"{\"query\":\"beta\"}"}}]}}]}`)
		default:
			_, _ = fmt.Fprint(w, `{"model":"deepseek-v4-flash","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`)
		}
	}))
	defer server.Close()

	events := runReasoningToolConversation(t, server.URL, adkagent.StreamingModeNone)

	mu.Lock()
	gotRequests := append([]ChatRequest(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 3 {
		t.Fatalf("requests = %d, want three model calls", len(gotRequests))
	}
	assertReasoningToolRoundTrip(t, gotRequests[1].Messages, "reason-one", "call-1", "alpha")
	assertReasoningToolRoundTrip(t, gotRequests[2].Messages, "reason-one", "call-1", "alpha")
	assertReasoningToolRoundTrip(t, gotRequests[2].Messages, "reason-two", "call-2", "beta")
	assertReasoningNotVisible(t, events, "reason-one", "reason-two")
}

func TestADKRunnerPreservesFragmentedStreamingReasoningContent(t *testing.T) {
	var mu sync.Mutex
	var requests []ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, req)
		requestNumber := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if requestNumber == 1 {
			_, _ = fmt.Fprint(w, "data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"delta\":{\"reasoning_content\":\"stream-rea\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"look\",\"arguments\":\"{\\\"query\\\":\\\"al\"}}]}}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{\"reasoning_content\":\"soning\",\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"up\",\"arguments\":\"pha\\\"}\"}}]}}]}\n\n")
		} else {
			_, _ = fmt.Fprint(w, "data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{\"content\":\"done\"}}]}\n\n")
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	events := runReasoningToolConversation(t, server.URL, adkagent.StreamingModeSSE)

	mu.Lock()
	gotRequests := append([]ChatRequest(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 2 {
		t.Fatalf("requests = %d, want two model calls", len(gotRequests))
	}
	assertReasoningToolRoundTrip(t, gotRequests[1].Messages, "stream-reasoning", "call-1", "alpha")
	assertReasoningNotVisible(t, events, "stream-reasoning")
}

func TestReasoningContentMappingContracts(t *testing.T) {
	t.Run("full streaming value replaces accumulated deltas", func(t *testing.T) {
		if got := mergeReasoningContent("partial-", "ignored", "complete"); got != "complete" {
			t.Fatalf("reasoning = %q, want complete", got)
		}
	})

	t.Run("ADK history mapping handles nils and marshal fallback", func(t *testing.T) {
		call := genai.NewPartFromFunctionCall("lookup.original", map[string]any{"query": "alpha"})
		call.FunctionCall.ID = "call-1"
		call.ThoughtSignature = []byte("opaque-reasoning")
		response := genai.NewPartFromFunctionResponse("lookup.original", map[string]any{"bad": make(chan int)})
		response.FunctionResponse.ID = "call-1"
		messages := contentsToMessages(
			[]*genai.Content{
				nil,
				{Role: genai.RoleModel, Parts: []*genai.Part{nil, call}},
				{Role: genai.RoleUser, Parts: []*genai.Part{response}},
				{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "hello", ThoughtSignature: []byte("must-not-map")}}},
			},
			"system prompt",
			map[string]string{"lookup.original": "lookup_wire"},
		)
		if len(messages) != 4 {
			t.Fatalf("messages = %+v, want system, assistant, fallback tool, and user shell", messages)
		}
		if messages[0].Role != "system" || messages[0].Content != "system prompt" {
			t.Fatalf("system message = %+v", messages[0])
		}
		assistant := messages[1]
		if assistant.Role != "assistant" || assistant.ReasoningContent != "opaque-reasoning" || len(assistant.ToolCalls) != 1 {
			t.Fatalf("assistant message = %+v", assistant)
		}
		if assistant.ToolCalls[0].Function.Name != "lookup_wire" {
			t.Fatalf("assistant tool = %+v", assistant.ToolCalls[0])
		}
		toolMessage := messages[2]
		fallback, ok := toolMessage.Content.(string)
		if toolMessage.Role != "tool" || toolMessage.Name != "lookup_wire" || !ok || !strings.HasPrefix(fallback, "map[bad:") {
			t.Fatalf("fallback tool message = %+v", toolMessage)
		}
		if messages[3].Role != "user" || messages[3].Content != "hello" || messages[3].ReasoningContent != "" {
			t.Fatalf("user message = %+v", messages[3])
		}
	})

	t.Run("empty provider response remains an empty completed response", func(t *testing.T) {
		response := responseToADK(ChatResponse{}, nil)
		if response == nil || response.Content == nil || len(response.Content.Parts) != 1 || response.Content.Parts[0].Text != "" {
			t.Fatalf("response = %+v", response)
		}
	})

	t.Run("reasoning is attached once across parallel tool calls", func(t *testing.T) {
		response := responseToADK(ChatResponse{Choices: []struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
			Delta        Message `json:"delta,omitempty"`
		}{{Message: Message{
			Role:             "assistant",
			ReasoningContent: "opaque-reasoning",
			ToolCalls: []ToolCall{
				{ID: "call-1", Function: ToolCallFunction{Name: "lookup", Arguments: `{}`}},
				{ID: "call-2", Function: ToolCallFunction{Name: "lookup", Arguments: `{}`}},
			},
		}}}}, nil)
		if len(response.Content.Parts) != 2 {
			t.Fatalf("parts = %+v", response.Content.Parts)
		}
		if got := string(response.Content.Parts[0].ThoughtSignature); got != "opaque-reasoning" {
			t.Fatalf("first signature = %q", got)
		}
		if len(response.Content.Parts[1].ThoughtSignature) != 0 {
			t.Fatalf("second signature = %q, want empty", response.Content.Parts[1].ThoughtSignature)
		}
	})

	t.Run("messages without reasoning remain unchanged", func(t *testing.T) {
		message := Message{
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: "call-1", Function: ToolCallFunction{Name: "lookup", Arguments: `{}`}}},
		}
		encoded, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "reasoning_content") {
			t.Fatalf("message unexpectedly contains reasoning_content: %s", encoded)
		}
		response := responseToADK(ChatResponse{Choices: []struct {
			Index        int     `json:"index"`
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
			Delta        Message `json:"delta,omitempty"`
		}{{Message: message}}}, nil)
		if len(response.Content.Parts) != 1 || len(response.Content.Parts[0].ThoughtSignature) != 0 {
			t.Fatalf("response = %+v, want ordinary unsigned tool call", response)
		}
	})
}

func runReasoningToolConversation(t *testing.T, baseURL string, streamingMode adkagent.StreamingMode) []*adksession.Event {
	t.Helper()
	model, err := NewADKModelWithThinking(
		ModelFlash,
		New(WithBaseURLForTest(baseURL), WithAPIKey("test-key")),
		Thinking{Type: "enabled"},
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := functiontool.New(
		functiontool.Config{Name: "lookup", Description: "look up a value"},
		func(_ adktool.Context, args reasoningLookupArgs) (map[string]any, error) {
			return map[string]any{"value": args.Query}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := llmagent.New(llmagent.Config{
		Name:  "reasoning_contract_agent",
		Model: model,
		Tools: []adktool.Tool{lookup},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runner.New(runner.Config{
		AppName:           "reasoning_contract",
		Agent:             agent,
		SessionService:    adksession.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var events []*adksession.Event
	for event, err := range run.Run(
		context.Background(),
		"user-1",
		"session-1",
		genai.NewContentFromText("look up alpha and beta", genai.RoleUser),
		adkagent.RunConfig{StreamingMode: streamingMode},
	) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if event != nil {
			events = append(events, event)
		}
	}
	return events
}

func assertReasoningToolRoundTrip(t *testing.T, messages []Message, reasoning, callID, query string) {
	t.Helper()
	var assistant *Message
	var toolResult *Message
	for i := range messages {
		message := &messages[i]
		if message.Role == "assistant" && len(message.ToolCalls) == 1 && message.ToolCalls[0].ID == callID {
			assistant = message
		}
		if message.Role == "tool" && message.ToolCallID == callID {
			toolResult = message
		}
	}
	if assistant == nil {
		t.Fatalf("assistant tool call %q missing from messages: %+v", callID, messages)
	}
	if assistant.ReasoningContent != reasoning {
		t.Fatalf("assistant reasoning_content = %#v, want %q", assistant.ReasoningContent, reasoning)
	}
	if assistant.ToolCalls[0].Function.Arguments != fmt.Sprintf(`{"query":%q}`, query) {
		t.Fatalf("assistant tool arguments = %q", assistant.ToolCalls[0].Function.Arguments)
	}
	if toolResult == nil {
		t.Fatalf("tool result %q missing from messages: %+v", callID, messages)
	}
	result, ok := toolResult.Content.(string)
	if !ok || !strings.Contains(result, fmt.Sprintf(`"value":%q`, query)) {
		t.Fatalf("tool result = %#v, want value %q", toolResult.Content, query)
	}
}

func assertReasoningNotVisible(t *testing.T, events []*adksession.Event, forbidden ...string) {
	t.Helper()
	for _, event := range events {
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part == nil {
				continue
			}
			for _, value := range forbidden {
				if strings.Contains(part.Text, value) {
					t.Fatalf("reasoning leaked into event text %q", part.Text)
				}
			}
		}
	}
}
