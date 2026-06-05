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
	Index    int              `json:"index,omitempty"`
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
	startedAt := time.Now()
	if !SupportedModel(req.Model) {
		err := fmt.Errorf("%w: %s", ErrUnsupportedModel, req.Model)
		traceCall(ctx, CallTrace{Request: req, Err: err, StartedAt: startedAt, EndedAt: time.Now()})
		return ChatResponse{}, err
	}
	if strings.TrimSpace(c.apiKey) == "" {
		err := errors.New("DEEPSEEK_API_KEY is required")
		traceCall(ctx, CallTrace{Request: req, Err: err, StartedAt: startedAt, EndedAt: time.Now()})
		return ChatResponse{}, err
	}
	req.Stream = false
	traceRequest(ctx, req)
	var out ChatResponse
	if err := c.do(ctx, req, &out); err != nil {
		traceCall(ctx, CallTrace{Request: req, Response: &out, Err: err, StartedAt: startedAt, EndedAt: time.Now()})
		return out, err
	}
	traceCall(ctx, CallTrace{Request: req, Response: &out, StartedAt: startedAt, EndedAt: time.Now()})
	return out, nil
}

func (c *Client) Stream(ctx context.Context, req ChatRequest, yield func(ChatResponse, error) bool) {
	startedAt := time.Now()
	req.Stream = true
	var lastResp *ChatResponse
	finish := func(err error) {
		traceCall(ctx, CallTrace{Request: req, Response: lastResp, Err: err, StartedAt: startedAt, EndedAt: time.Now()})
	}
	if !SupportedModel(req.Model) {
		err := fmt.Errorf("%w: %s", ErrUnsupportedModel, req.Model)
		yield(ChatResponse{}, err)
		finish(err)
		return
	}
	if strings.TrimSpace(c.apiKey) == "" {
		err := errors.New("DEEPSEEK_API_KEY is required")
		yield(ChatResponse{}, err)
		finish(err)
		return
	}
	traceRequest(ctx, req)
	body, err := json.Marshal(req)
	if err != nil {
		yield(ChatResponse{}, err)
		finish(err)
		return
	}
	url := c.baseURL + "/chat/completions"
	traceRaw(ctx, RawTrace{Event: "request_body", Method: http.MethodPost, URL: url, Body: body})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		yield(ChatResponse{}, err)
		finish(err)
		return
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		yield(ChatResponse{}, err)
		finish(err)
		return
	}
	defer resp.Body.Close()
	traceRaw(ctx, RawTrace{Event: "response_status", StatusCode: resp.StatusCode, Header: resp.Header.Clone()})
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			traceRaw(ctx, RawTrace{Event: "response_error", Error: readErr.Error()})
		}
		traceRaw(ctx, RawTrace{Event: "response_body", Body: data})
		err := fmt.Errorf("deepseek stream failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(data)))
		yield(ChatResponse{}, err)
		finish(err)
		return
	}
	reader := bufio.NewReader(resp.Body)
	var rawLines []string
	var sawData bool
	finishReadErr := func(err error) bool {
		if err == nil || err == io.EOF {
			return false
		}
		traceRaw(ctx, RawTrace{Event: "response_error", Error: err.Error()})
		yield(ChatResponse{}, err)
		finish(err)
		return true
	}
	for {
		rawLine, readErr := reader.ReadString('\n')
		if rawLine != "" {
			traceRaw(ctx, RawTrace{Event: "response_chunk", Body: []byte(rawLine)})
		}
		if rawLine == "" && readErr == io.EOF {
			break
		}
		if rawLine == "" && readErr != nil {
			traceRaw(ctx, RawTrace{Event: "response_error", Error: readErr.Error()})
			yield(ChatResponse{}, readErr)
			finish(readErr)
			return
		}
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, ":") {
			if readErr == io.EOF {
				break
			}
			if finishReadErr(readErr) {
				return
			}
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			rawLines = append(rawLines, line)
			if readErr == io.EOF {
				break
			}
			if finishReadErr(readErr) {
				return
			}
			continue
		}
		sawData = true
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			finish(nil)
			return
		}
		var chunk ChatResponse
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			if !yield(ChatResponse{}, err) {
				finish(err)
				return
			}
			continue
		}
		lastResp = &chunk
		if !yield(chunk, nil) {
			finish(nil)
			return
		}
		if readErr == io.EOF {
			break
		}
		if finishReadErr(readErr) {
			return
		}
	}
	if !sawData && len(rawLines) > 0 {
		var full ChatResponse
		if err := json.Unmarshal([]byte(strings.Join(rawLines, "\n")), &full); err != nil {
			yield(ChatResponse{}, err)
			finish(err)
			return
		}
		lastResp = &full
		if !yield(full, nil) {
			finish(nil)
			return
		}
	}
	finish(nil)
}

func (c *Client) do(ctx context.Context, req ChatRequest, out any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	url := c.baseURL + "/chat/completions"
	traceRaw(ctx, RawTrace{Event: "request_body", Method: http.MethodPost, URL: url, Body: body})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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
	traceRaw(ctx, RawTrace{Event: "response_status", StatusCode: resp.StatusCode, Header: resp.Header.Clone()})
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		traceRaw(ctx, RawTrace{Event: "response_error", Error: err.Error()})
		return err
	}
	traceRaw(ctx, RawTrace{Event: "response_body", Body: data})
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("deepseek chat failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

type ADKModel struct {
	modelName string
	client    *Client
	thinking  *Thinking
}

func NewADKModel(modelName string, client *Client) (*ADKModel, error) {
	return NewADKModelWithThinking(modelName, client, Thinking{Type: "disabled"})
}

func NewADKModelWithThinking(modelName string, client *Client, thinking Thinking) (*ADKModel, error) {
	if !SupportedModel(modelName) {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedModel, modelName)
	}
	if client == nil {
		client = New()
	}
	if strings.TrimSpace(thinking.Type) == "" {
		thinking.Type = "disabled"
	}
	return &ADKModel{modelName: modelName, client: client, thinking: &thinking}, nil
}

func cloneThinking(thinking *Thinking) *Thinking {
	if thinking == nil {
		return nil
	}
	return &Thinking{Type: thinking.Type}
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
			Messages: contentsToMessages(req.Contents, systemInstruction(req), originalToWire),
			Tools:    tools,
			Thinking: cloneThinking(m.thinking),
		}
		if req.Model != "" && SupportedModel(req.Model) {
			chatReq.Model = req.Model
		}
		if req.Config != nil && req.Config.Temperature != nil {
			t := float32(*req.Config.Temperature)
			chatReq.Temperature = &t
		}
		if stream {
			stopped := false
			var full strings.Builder
			var streamedToolCalls []ToolCall
			var lastModel string
			var lastFinishReason string
			m.client.Stream(ctx, chatReq, func(chunk ChatResponse, err error) bool {
				if err != nil {
					stopped = !yield(nil, err)
					return !stopped
				}
				if strings.TrimSpace(chunk.Model) != "" {
					lastModel = chunk.Model
				}
				if len(chunk.Choices) > 0 {
					if strings.TrimSpace(chunk.Choices[0].FinishReason) != "" {
						lastFinishReason = chunk.Choices[0].FinishReason
					}
					streamedToolCalls = mergeToolCallDeltas(streamedToolCalls, chunk.Choices[0].Delta.ToolCalls)
					streamedToolCalls = mergeFullToolCalls(streamedToolCalls, chunk.Choices[0].Message.ToolCalls)
				}
				text := chunkText(chunk)
				if text == "" {
					return true
				}
				full.WriteString(text)
				stopped = !yield(textResponse(text, true), nil)
				return !stopped
			})
			if stopped {
				return
			}
			if len(streamedToolCalls) > 0 {
				if !yield(responseToADK(ChatResponse{
					Model: lastModel,
					Choices: []struct {
						Index        int     `json:"index"`
						Message      Message `json:"message"`
						FinishReason string  `json:"finish_reason"`
						Delta        Message `json:"delta,omitempty"`
					}{{
						Message:      Message{Role: "assistant", ToolCalls: streamedToolCalls},
						FinishReason: "tool_calls",
					}},
				}, wireToOriginal), nil) {
					return
				}
			} else if strings.TrimSpace(full.String()) != "" {
				resp := textResponse(full.String(), false)
				resp.ModelVersion = lastModel
				resp.FinishReason = genai.FinishReason(lastFinishReason)
				if !yield(resp, nil) {
					return
				}
			} else {
				if !yield(&adkmodel.LLMResponse{TurnComplete: true}, nil) {
					return
				}
			}
			return
		}
		resp, err := m.client.Chat(ctx, chatReq)
		if err != nil {
			_ = yield(nil, err)
			return
		}
		_ = yield(responseToADK(resp, wireToOriginal), nil)
	}
}

func contentsToMessages(contents []*genai.Content, systemInstruction string, originalToWire map[string]string) []Message {
	out := make([]Message, 0, len(contents)+1)
	if systemInstruction != "" {
		out = append(out, Message{Role: "system", Content: systemInstruction})
	}
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

func systemInstruction(req *adkmodel.LLMRequest) string {
	if req == nil || req.Config == nil {
		return ""
	}
	return genaiContentText(req.Config.SystemInstruction)
}

func genaiContentText(content *genai.Content) string {
	if content == nil {
		return ""
	}
	parts := make([]string, 0, len(content.Parts))
	for _, part := range content.Parts {
		if part != nil && strings.TrimSpace(part.Text) != "" {
			parts = append(parts, strings.TrimSpace(part.Text))
		}
	}
	return strings.Join(parts, "\n")
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

func mergeToolCallDeltas(existing, deltas []ToolCall) []ToolCall {
	return mergeToolCalls(existing, deltas, true)
}

func mergeFullToolCalls(existing, calls []ToolCall) []ToolCall {
	return mergeToolCalls(existing, calls, false)
}

func mergeToolCalls(existing, calls []ToolCall, appendFragments bool) []ToolCall {
	for _, call := range calls {
		index := call.Index
		for len(existing) <= index {
			existing = append(existing, ToolCall{Index: len(existing), Type: "function"})
		}
		current := existing[index]
		if strings.TrimSpace(call.ID) != "" {
			current.ID = call.ID
		}
		if strings.TrimSpace(call.Type) != "" {
			current.Type = call.Type
		}
		if strings.TrimSpace(call.Function.Name) != "" {
			if appendFragments {
				current.Function.Name += call.Function.Name
			} else {
				current.Function.Name = call.Function.Name
			}
		}
		if call.Function.Arguments != "" {
			if appendFragments {
				current.Function.Arguments += call.Function.Arguments
			} else {
				current.Function.Arguments = call.Function.Arguments
			}
		}
		existing[index] = current
	}
	return existing
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
