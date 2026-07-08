package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestSupportedModelWhitelist(t *testing.T) {
	for _, model := range []string{ModelFlash, ModelPro} {
		if !SupportedModel(model) {
			t.Fatalf("%s should be supported", model)
		}
	}
	if SupportedModel("deepseek-chat") {
		t.Fatal("deepseek-chat should not be supported in phase 1")
	}
}

func TestClientChatWithFakeServer(t *testing.T) {
	var gotAuth string
	var gotReq ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model:    ModelFlash,
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth header = %q", gotAuth)
	}
	if gotReq.Model != ModelFlash {
		t.Fatalf("model = %q", gotReq.Model)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("content = %#v", resp.Choices[0].Message.Content)
	}
}

func TestClientChatToolCallsWithFakeServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"command_run","arguments":"{\"command\":\"pwd\"}"}}]}}]}`))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model:    ModelFlash,
		Messages: []Message{{Role: "user", Content: "search"}},
		Tools:    []Tool{{Type: "function", Function: ToolFunction{Name: "command_run"}}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "command_run" {
		t.Fatalf("tool calls = %+v", calls)
	}
}

func TestDeepSeekToolNameSanitizesDots(t *testing.T) {
	if got := DeepSeekToolName("command.run"); got != "command_run" {
		t.Fatalf("tool name = %q", got)
	}
	if got := DeepSeekToolName("mcp.echo"); got != "mcp_echo" {
		t.Fatalf("tool name = %q", got)
	}
}

// TestGenaiToolsToDeepSeekPreservesParametersJsonSchema 验证 #143 根因修复：
// ADK functiontool 把 schema 填在 ParametersJsonSchema（jsonschema），不是 Parameters（genai.Schema）。
// genaiToolsToDeepSeek 必须读 ParametersJsonSchema，否则工具定义发给 DeepSeek 时丢了 parameters，
// LLM 不知道要传哪些字段（如 human.request 的 question）→ 产空 arguments → validation 失败。
func TestGenaiToolsToDeepSeekPreservesParametersJsonSchema(t *testing.T) {
	// 模拟 ADK functiontool.Declaration() 产出的 FunctionDeclaration：
	// 只填 ParametersJsonSchema（jsonschema dict），Parameters 为 nil。
	tools := []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "human.request",
			Description: "ask a human",
			ParametersJsonSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{"type": "string"},
				},
				"required": []any{"question"},
			},
		}},
	}}

	out, _, _ := genaiToolsToDeepSeek(tools)
	if len(out) != 1 {
		t.Fatalf("got %d tools, want 1", len(out))
	}
	params := out[0].Function.Parameters
	if params == nil {
		t.Fatal("Parameters is nil — genaiToolsToDeepSeek did not read ParametersJsonSchema (the #143 root cause)")
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type in Parameters: %#v", params)
	}
	if _, ok := props["question"]; !ok {
		t.Errorf("question property missing from Parameters: %#v", params)
	}
	required, _ := params["required"].([]any)
	foundQuestion := false
	for _, r := range required {
		if r == "question" {
			foundQuestion = true
		}
	}
	if !foundQuestion {
		t.Errorf("question not in required %v: %#v", required, params)
	}
}

// TestGenaiToolsToDeepSeekStillReadsLegacyParameters 验证 fallback：
// 旧的 Parameters（*genai.Schema）路径仍兼容（不该因加 ParametersJsonSchema 支持而破坏）。
func TestGenaiToolsToDeepSeekStillReadsLegacyParameters(t *testing.T) {
	tools := []*genai.Tool{{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "legacy.tool",
			Description: "legacy",
			Parameters: &genai.Schema{
				Type: "object",
				Properties: map[string]*genai.Schema{
					"x": {Type: "string"},
				},
			},
		}},
	}}
	out, _, _ := genaiToolsToDeepSeek(tools)
	if len(out) != 1 {
		t.Fatalf("got %d tools, want 1", len(out))
	}
	props, ok := out[0].Function.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("legacy Parameters not converted: %#v", out[0].Function.Parameters)
	}
	if _, ok := props["x"]; !ok {
		t.Errorf("property x missing from legacy Parameters: %#v", props)
	}
}

func TestMergeToolCallDeltasAppendsFunctionNameFragments(t *testing.T) {
	calls := mergeToolCallDeltas(nil, []ToolCall{{
		Index: 0,
		ID:    "call-1",
		Type:  "function",
		Function: ToolCallFunction{
			Name:      "read_",
			Arguments: `{"pa`,
		},
	}})
	calls = mergeToolCallDeltas(calls, []ToolCall{{
		Index: 0,
		Function: ToolCallFunction{
			Name:      "file",
			Arguments: `th":"agents/PROFILE.md"}`,
		},
	}})
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v", calls)
	}
	if calls[0].Function.Name != "read_file" {
		t.Fatalf("tool call name = %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"path":"agents/PROFILE.md"}` {
		t.Fatalf("tool call arguments = %q", calls[0].Function.Arguments)
	}
}

func TestMergeToolCallDeltasPreservesWhitespaceArgumentFragments(t *testing.T) {
	calls := mergeToolCallDeltas(nil, []ToolCall{{
		Index: 0,
		ID:    "call-1",
		Type:  "function",
		Function: ToolCallFunction{
			Name:      "shell_run",
			Arguments: `{"command":"wxview contacts --format json`,
		},
	}})
	for _, fragment := range []string{" ", "2>/dev/null\"}"} {
		calls = mergeToolCallDeltas(calls, []ToolCall{{
			Index: 0,
			Function: ToolCallFunction{
				Arguments: fragment,
			},
		}})
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v", calls)
	}
	if calls[0].Function.Arguments != `{"command":"wxview contacts --format json 2>/dev/null"}` {
		t.Fatalf("tool call arguments = %q", calls[0].Function.Arguments)
	}
}

func TestMergeFullToolCallsReplacesFunctionFields(t *testing.T) {
	calls := mergeFullToolCalls(nil, []ToolCall{{
		Index: 0,
		ID:    "call-1",
		Type:  "function",
		Function: ToolCallFunction{
			Name:      "read_file",
			Arguments: `{"path":"agents/PROFILE.md"}`,
		},
	}})
	calls = mergeFullToolCalls(calls, []ToolCall{{
		Index: 0,
		ID:    "call-1",
		Type:  "function",
		Function: ToolCallFunction{
			Name:      "read_file",
			Arguments: `{"path":"agents/PROFILE.md"}`,
		},
	}})
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v", calls)
	}
	if calls[0].Function.Name != "read_file" {
		t.Fatalf("tool call name = %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"path":"agents/PROFILE.md"}` {
		t.Fatalf("tool call arguments = %q", calls[0].Function.Arguments)
	}
}

func TestClientStreamWithFakeServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	var chunks []string
	client.Stream(context.Background(), ChatRequest{Model: ModelFlash, Messages: []Message{{Role: "user", Content: "hi"}}}, func(resp ChatResponse, err error) bool {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		chunks = append(chunks, chunkText(resp))
		return true
	})
	if strings.Join(chunks, "") != "hello" {
		t.Fatalf("chunks = %q", strings.Join(chunks, ""))
	}
}

func TestClientStreamRecordsRawRequestAndResponseChunks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	var traces []RawTrace
	ctx := WithRawTraceRecorder(context.Background(), func(_ context.Context, trace RawTrace) {
		traces = append(traces, trace)
	})
	client.Stream(ctx, ChatRequest{Model: ModelFlash, Messages: []Message{{Role: "user", Content: "hi"}}}, func(resp ChatResponse, err error) bool {
		if err != nil {
			t.Fatalf("stream err: %v", err)
		}
		return true
	})

	var requestBody, status, firstChunk, doneChunk bool
	for _, trace := range traces {
		switch trace.Event {
		case "request_body":
			requestBody = strings.Contains(string(trace.Body), `"stream":true`) && strings.Contains(string(trace.Body), `"hi"`)
		case "response_status":
			status = trace.StatusCode == http.StatusOK
		case "response_chunk":
			if strings.Contains(string(trace.Body), `"he"`) {
				firstChunk = true
			}
			if strings.Contains(string(trace.Body), "[DONE]") {
				doneChunk = true
			}
		}
	}
	if !requestBody || !status || !firstChunk || !doneChunk {
		t.Fatalf("raw traces = %+v, want request body, status, and response chunks", traces)
	}
}

func TestADKModelGenerateContent(t *testing.T) {
	var gotReq ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"role":"assistant","content":"adk ok"}}]}`))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	model, err := NewADKModel(ModelFlash, client)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}
	var got string
	for resp, err := range model.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if resp.Content != nil && len(resp.Content.Parts) > 0 {
			got = resp.Content.Parts[0].Text
		}
	}
	if got != "adk ok" {
		t.Fatalf("got %q", got)
	}
	if gotReq.Thinking == nil || gotReq.Thinking.Type != "disabled" {
		t.Fatalf("thinking = %+v, want disabled", gotReq.Thinking)
	}
}

func TestADKModelStreamStopsAfterConsumerBreak(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"delta\":{\"content\":\"second\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	model, err := NewADKModel(ModelFlash, client)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}
	var got string
	var count int
	for resp, err := range model.GenerateContent(context.Background(), req, true) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		count++
		if resp.Content != nil && len(resp.Content.Parts) > 0 {
			got = resp.Content.Parts[0].Text
		}
		break
	}
	if count != 1 || got != "first" {
		t.Fatalf("stream responses count=%d got=%q, want one partial response", count, got)
	}
}

func TestADKModelIncludesSystemInstruction(t *testing.T) {
	var gotReq ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	model, err := NewADKModel(ModelFlash, client)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("你是谁", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText("你是养生一号，不要自称 DeepSeek。", genai.RoleUser),
		},
	}
	for _, err := range model.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
	}
	if len(gotReq.Messages) != 2 {
		t.Fatalf("messages = %+v, want system and user", gotReq.Messages)
	}
	if gotReq.Messages[0].Role != "system" || gotReq.Messages[0].Content != "你是养生一号，不要自称 DeepSeek。" {
		t.Fatalf("system message = %+v", gotReq.Messages[0])
	}
	if gotReq.Messages[1].Role != "user" || gotReq.Messages[1].Content != "你是谁" {
		t.Fatalf("user message = %+v", gotReq.Messages[1])
	}
}

func TestADKModelExtractsStructuredContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"structured ok"}]}}]}`))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	model, err := NewADKModel(ModelFlash, client)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}
	var got string
	for resp, err := range model.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if resp.Content != nil && len(resp.Content.Parts) > 0 {
			got = resp.Content.Parts[0].Text
		}
	}
	if got != "structured ok" {
		t.Fatalf("got %q", got)
	}
}

func TestADKModelMapsToolNamesForDeepSeek(t *testing.T) {
	var gotReq ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"command_run","arguments":"{\"command\":\"pwd\"}"}}]}}]}`))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	model, err := NewADKModel(ModelFlash, client)
	if err != nil {
		t.Fatal(err)
	}
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("search", genai.RoleUser)},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{{
					Name:        "command.run",
					Description: "run a command",
				}},
			}},
		},
	}
	var gotCallName string
	for resp, err := range model.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if resp.Content != nil && len(resp.Content.Parts) > 0 && resp.Content.Parts[0].FunctionCall != nil {
			gotCallName = resp.Content.Parts[0].FunctionCall.Name
		}
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "command_run" {
		t.Fatalf("wire tool name = %+v", gotReq.Tools)
	}
	if gotCallName != "command.run" {
		t.Fatalf("ADK tool call name = %q", gotCallName)
	}
}

func TestADKModelSerializesFunctionResponseContent(t *testing.T) {
	var gotReq ChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	defer server.Close()
	client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
	model, err := NewADKModel(ModelFlash, client)
	if err != nil {
		t.Fatal(err)
	}
	response := genai.NewPartFromFunctionResponse("command.run", map[string]any{"ok": true, "count": 1})
	response.FunctionResponse.ID = "call-1"
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			genai.NewContentFromText("search", genai.RoleUser),
			genai.NewContentFromParts([]*genai.Part{response}, genai.RoleModel),
		},
		Config: &genai.GenerateContentConfig{
			Tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{{
					Name: "command.run",
				}},
			}},
		},
	}
	for _, err := range model.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
	}
	if len(gotReq.Messages) != 2 {
		t.Fatalf("messages = %+v", gotReq.Messages)
	}
	toolMessage := gotReq.Messages[1]
	if toolMessage.Role != "tool" || toolMessage.Name != "command_run" {
		t.Fatalf("tool message = %+v", toolMessage)
	}
	content, ok := toolMessage.Content.(string)
	if !ok {
		t.Fatalf("tool content type = %T", toolMessage.Content)
	}
	if !strings.Contains(content, `"ok":true`) {
		t.Fatalf("tool content = %q", content)
	}
}
