package deepseek

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
	"iter"
)

const (
	DefaultBaseURL = "https://api.deepseek.com"
	ModelFlash     = "deepseek-v4-flash"
	ModelPro       = "deepseek-v4-pro"
)

var ErrUnsupportedModel = errors.New("unsupported deepseek model")

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

type Option func(*Client)

func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.httpClient = client
		}
	}
}

func WithAPIKey(apiKey string) Option {
	return func(c *Client) {
		c.apiKey = apiKey
	}
}

func WithBaseURLForTest(baseURL string) Option {
	return func(c *Client) {
		if strings.TrimSpace(baseURL) != "" {
			c.baseURL = strings.TrimRight(baseURL, "/")
		}
	}
}

func New(opts ...Option) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		apiKey:  os.Getenv("DEEPSEEK_API_KEY"),
		httpClient: &http.Client{
			Timeout: 90 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func SupportedModel(model string) bool {
	return model == ModelFlash || model == ModelPro
}

type Message struct {
	Role             string     `json:"role"`
	Content          any        `json:"content,omitempty"`
	ReasoningContent any        `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Name             string     `json:"name,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	Tools       []Tool    `json:"tools,omitempty"`
	Temperature *float32  `json:"temperature,omitempty"`
	Thinking    *Thinking `json:"thinking,omitempty"`
}

type Thinking struct {
	Type string `json:"type"`
}

type ChatResponse struct {
	ID      string `json:"id,omitempty"`
	Model   string `json:"model,omitempty"`
	Choices []struct {
		Index        int     `json:"index"`
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
		Delta        Message `json:"delta,omitempty"`
	} `json:"choices"`
	Usage map[string]any `json:"usage,omitempty"`
}

func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if !SupportedModel(req.Model) {
		return ChatResponse{}, fmt.Errorf("%w: %s", ErrUnsupportedModel, req.Model)
	}
	if strings.TrimSpace(c.apiKey) == "" {
		return ChatResponse{}, errors.New("DEEPSEEK_API_KEY is required")
	}
	req.Stream = false
	var out ChatResponse
	if err := c.do(ctx, req, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) Stream(ctx context.Context, req ChatRequest, yield func(ChatResponse, error) bool) {
	if !SupportedModel(req.Model) {
		yield(ChatResponse{}, fmt.Errorf("%w: %s", ErrUnsupportedModel, req.Model))
		return
	}
	if strings.TrimSpace(c.apiKey) == "" {
		yield(ChatResponse{}, errors.New("DEEPSEEK_API_KEY is required"))
		return
	}
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		yield(ChatResponse{}, err)
		return
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		yield(ChatResponse{}, err)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		yield(ChatResponse{}, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		yield(ChatResponse{}, fmt.Errorf("deepseek stream failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(data))))
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return
		}
		var chunk ChatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			if !yield(ChatResponse{}, err) {
				return
			}
			continue
		}
		if !yield(chunk, nil) {
			return
		}
	}
	if err := scanner.Err(); err != nil {
		yield(ChatResponse{}, err)
	}
}

func (c *Client) do(ctx context.Context, req ChatRequest, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("deepseek chat failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

type ADKModel struct {
	modelName string
	client    *Client
}

func NewADKModel(modelName string, client *Client) (*ADKModel, error) {
	if !SupportedModel(modelName) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedModel, modelName)
	}
	if client == nil {
		client = New()
	}
	return &ADKModel{modelName: modelName, client: client}, nil
}

func (m *ADKModel) Name() string {
	return m.modelName
}

func (m *ADKModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		wireToOriginal := map[string]string{}
		originalToWire := map[string]string{}
		var tools []Tool
		if req.Config != nil {
			tools, wireToOriginal, originalToWire = genaiToolsToDeepSeek(req.Config.Tools)
		}
		chatReq := ChatRequest{
			Model:    m.modelName,
			Messages: contentsToMessages(req.Contents, originalToWire),
			Tools:    tools,
			Thinking: &Thinking{Type: "disabled"},
		}
		if req.Model != "" && SupportedModel(req.Model) {
			chatReq.Model = req.Model
		}
		if req.Config != nil && req.Config.Temperature != nil {
			t := float32(*req.Config.Temperature)
			chatReq.Temperature = &t
		}
		if stream {
			m.client.Stream(ctx, chatReq, func(chunk ChatResponse, err error) bool {
				if err != nil {
					return yield(nil, err)
				}
				text := chunkText(chunk)
				if text == "" {
					return true
				}
				return yield(textResponse(text, true), nil)
			})
			yield(&adkmodel.LLMResponse{TurnComplete: true}, nil)
			return
		}
		resp, err := m.client.Chat(ctx, chatReq)
		if err != nil {
			yield(nil, err)
			return
		}
		yield(responseToADK(resp, wireToOriginal), nil)
	}
}

func contentsToMessages(contents []*genai.Content, originalToWire map[string]string) []Message {
	out := make([]Message, 0, len(contents))
	for _, content := range contents {
		if content == nil {
			continue
		}
		role := "user"
		if strings.EqualFold(string(content.Role), "model") {
			role = "assistant"
		}
		var parts []string
		var toolCalls []ToolCall
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			if part.Text != "" {
				parts = append(parts, part.Text)
			}
			if part.FunctionCall != nil {
				args, _ := json.Marshal(part.FunctionCall.Args)
				name := part.FunctionCall.Name
				if originalToWire != nil && originalToWire[name] != "" {
					name = originalToWire[name]
				}
				toolCalls = append(toolCalls, ToolCall{
					ID:   part.FunctionCall.ID,
					Type: "function",
					Function: ToolCallFunction{
						Name:      name,
						Arguments: string(args),
					},
				})
			}
			if part.FunctionResponse != nil {
				name := part.FunctionResponse.Name
				if originalToWire != nil && originalToWire[name] != "" {
					name = originalToWire[name]
				}
				content, err := json.Marshal(part.FunctionResponse.Response)
				if err != nil {
					content = []byte(fmt.Sprint(part.FunctionResponse.Response))
				}
				out = append(out, Message{
					Role:       "tool",
					ToolCallID: part.FunctionResponse.ID,
					Name:       name,
					Content:    string(content),
				})
			}
		}
		if len(parts) > 0 || len(toolCalls) > 0 {
			out = append(out, Message{Role: role, Content: strings.Join(parts, "\n"), ToolCalls: toolCalls})
		}
	}
	return out
}

func genaiToolsToDeepSeek(tools []*genai.Tool) ([]Tool, map[string]string, map[string]string) {
	var out []Tool
	wireToOriginal := map[string]string{}
	originalToWire := map[string]string{}
	for _, t := range tools {
		if t == nil {
			continue
		}
		for _, fn := range t.FunctionDeclarations {
			if fn == nil {
				continue
			}
			params := map[string]any{}
			if fn.Parameters != nil {
				raw, _ := json.Marshal(fn.Parameters)
				_ = json.Unmarshal(raw, &params)
			}
			wireName := DeepSeekToolName(fn.Name)
			wireToOriginal[wireName] = fn.Name
			originalToWire[fn.Name] = wireName
			out = append(out, Tool{
				Type: "function",
				Function: ToolFunction{
					Name:        wireName,
					Description: fn.Description,
					Parameters:  params,
				},
			})
		}
	}
	return out, wireToOriginal, originalToWire
}

func responseToADK(resp ChatResponse, wireToOriginal map[string]string) *adkmodel.LLMResponse {
	if len(resp.Choices) == 0 {
		return textResponse("", false)
	}
	choice := resp.Choices[0]
	msg := choice.Message
	var parts []*genai.Part
	if text := ContentText(msg.Content); text != "" {
		parts = append(parts, genai.NewPartFromText(text))
	}
	for _, call := range msg.ToolCalls {
		args := map[string]any{}
		_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
		name := call.Function.Name
		if wireToOriginal != nil && wireToOriginal[name] != "" {
			name = wireToOriginal[name]
		}
		part := genai.NewPartFromFunctionCall(name, args)
		part.FunctionCall.ID = call.ID
		parts = append(parts, part)
	}
	return &adkmodel.LLMResponse{
		Content:      genai.NewContentFromParts(parts, genai.RoleModel),
		TurnComplete: true,
		ModelVersion: resp.Model,
		FinishReason: genai.FinishReason(choice.FinishReason),
	}
}

func textResponse(text string, partial bool) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content:      genai.NewContentFromText(text, genai.RoleModel),
		Partial:      partial,
		TurnComplete: !partial,
	}
}

func chunkText(chunk ChatResponse) string {
	if len(chunk.Choices) == 0 {
		return ""
	}
	if text := ContentText(chunk.Choices[0].Delta.Content); text != "" {
		return text
	}
	if text := ContentText(chunk.Choices[0].Message.Content); text != "" {
		return text
	}
	return ""
}

func ContentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if text := ContentText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	case map[string]any:
		for _, key := range []string{"text", "content", "value"} {
			if text := ContentText(v[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func DeepSeekToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "tool"
	}
	var b strings.Builder
	for _, r := range name {
		if r == '_' || r == '-' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "tool"
	}
	first := rune(out[0])
	if !(first == '_' || first == '-' || unicode.IsLetter(first) || unicode.IsDigit(first)) {
		return "tool_" + out
	}
	return out
}
