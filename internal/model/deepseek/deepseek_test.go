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
