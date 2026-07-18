package deepseek

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type errorReadCloser struct {
	err error
}

func (r errorReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (errorReadCloser) Close() error               { return nil }

func TestClientChatFailureContracts(t *testing.T) {
	req := ChatRequest{Model: ModelFlash, Messages: []Message{{Role: "user", Content: "hi"}}}

	t.Run("unsupported model is rejected before transport", func(t *testing.T) {
		var trace CallTrace
		ctx := WithCallTraceRecorder(context.Background(), func(_ context.Context, got CallTrace) {
			trace = got
		})
		client := New(WithAPIKey("test-key"))
		_, err := client.Chat(ctx, ChatRequest{Model: "deepseek-unknown"})
		if !errors.Is(err, ErrUnsupportedModel) {
			t.Fatalf("error = %v, want ErrUnsupportedModel", err)
		}
		if !errors.Is(trace.Err, ErrUnsupportedModel) || trace.StartedAt.IsZero() || trace.EndedAt.IsZero() {
			t.Fatalf("call trace = %+v, want timed unsupported-model failure", trace)
		}
	})

	t.Run("missing API key is rejected before transport", func(t *testing.T) {
		client := New(WithAPIKey(" "))
		_, err := client.Chat(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "DEEPSEEK_API_KEY") {
			t.Fatalf("error = %v, want missing-key failure", err)
		}
	})

	t.Run("provider rejection includes status and body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
		}))
		defer server.Close()
		client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
		_, err := client.Chat(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "status=429") || !strings.Contains(err.Error(), "rate limited") {
			t.Fatalf("error = %v, want provider status and body", err)
		}
	})

	t.Run("malformed success body is rejected", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not-json`))
		}))
		defer server.Close()
		client := New(WithBaseURLForTest(server.URL), WithAPIKey("test-key"))
		_, err := client.Chat(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "invalid character") {
			t.Fatalf("error = %v, want JSON decode failure", err)
		}
	})

	t.Run("transport failure is returned", func(t *testing.T) {
		transportErr := errors.New("transport unavailable")
		client := New(
			WithAPIKey("test-key"),
			WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, transportErr
			})}),
		)
		_, err := client.Chat(context.Background(), req)
		if !errors.Is(err, transportErr) {
			t.Fatalf("error = %v, want transport error", err)
		}
	})

	t.Run("response read failure is returned and traced", func(t *testing.T) {
		readErr := errors.New("body read failed")
		client := New(
			WithAPIKey("test-key"),
			WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       errorReadCloser{err: readErr},
				}, nil
			})}),
		)
		var raw []RawTrace
		ctx := WithRawTraceRecorder(context.Background(), func(_ context.Context, trace RawTrace) {
			raw = append(raw, trace)
		})
		_, err := client.Chat(ctx, req)
		if !errors.Is(err, readErr) {
			t.Fatalf("error = %v, want response read error", err)
		}
		if len(raw) == 0 || raw[len(raw)-1].Event != "response_error" || raw[len(raw)-1].Error != readErr.Error() {
			t.Fatalf("raw traces = %+v, want response_error", raw)
		}
	})
}

func TestClientStreamFailureAndFallbackContracts(t *testing.T) {
	req := ChatRequest{Model: ModelFlash, Messages: []Message{{Role: "user", Content: "hi"}}}

	collect := func(client *Client, request ChatRequest) ([]ChatResponse, []error) {
		t.Helper()
		var responses []ChatResponse
		var errs []error
		client.Stream(context.Background(), request, func(resp ChatResponse, err error) bool {
			if err != nil {
				errs = append(errs, err)
			} else {
				responses = append(responses, resp)
			}
			return true
		})
		return responses, errs
	}

	t.Run("request guards surface through callback", func(t *testing.T) {
		_, errs := collect(New(WithAPIKey("test-key")), ChatRequest{Model: "deepseek-unknown"})
		if len(errs) != 1 || !errors.Is(errs[0], ErrUnsupportedModel) {
			t.Fatalf("errors = %v, want unsupported model", errs)
		}

		_, errs = collect(New(WithAPIKey(" ")), req)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "DEEPSEEK_API_KEY") {
			t.Fatalf("errors = %v, want missing API key", errs)
		}
	})

	t.Run("invalid endpoint and transport failures surface through callback", func(t *testing.T) {
		_, errs := collect(New(WithBaseURLForTest("://bad"), WithAPIKey("test-key")), req)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "missing protocol scheme") {
			t.Fatalf("errors = %v, want invalid endpoint", errs)
		}

		transportErr := errors.New("stream transport unavailable")
		client := New(
			WithAPIKey("test-key"),
			WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, transportErr
			})}),
		)
		_, errs = collect(client, req)
		if len(errs) != 1 || !errors.Is(errs[0], transportErr) {
			t.Fatalf("errors = %v, want transport failure", errs)
		}
	})

	t.Run("provider rejection includes status and body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("upstream unavailable"))
		}))
		defer server.Close()
		_, errs := collect(New(WithBaseURLForTest(server.URL), WithAPIKey("test-key")), req)
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "status=502") || !strings.Contains(errs[0].Error(), "upstream unavailable") {
			t.Fatalf("errors = %v, want provider status and body", errs)
		}
	})

	t.Run("malformed SSE event is reported without losing later events", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: not-json\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"recovered\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer server.Close()
		responses, errs := collect(New(WithBaseURLForTest(server.URL), WithAPIKey("test-key")), req)
		if len(errs) != 1 || len(responses) != 1 || chunkText(responses[0]) != "recovered" {
			t.Fatalf("responses=%+v errors=%v, want one decode error then recovered event", responses, errs)
		}
	})

	t.Run("non-SSE full response is accepted", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"model":"deepseek-v4-flash","choices":[{"message":{"content":"fallback"}}]}`))
		}))
		defer server.Close()
		responses, errs := collect(New(WithBaseURLForTest(server.URL), WithAPIKey("test-key")), req)
		if len(errs) != 0 || len(responses) != 1 || chunkText(responses[0]) != "fallback" {
			t.Fatalf("responses=%+v errors=%v, want decoded fallback response", responses, errs)
		}
	})

	t.Run("malformed non-SSE response is rejected", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not-json"))
		}))
		defer server.Close()
		responses, errs := collect(New(WithBaseURLForTest(server.URL), WithAPIKey("test-key")), req)
		if len(responses) != 0 || len(errs) != 1 {
			t.Fatalf("responses=%+v errors=%v, want malformed fallback error", responses, errs)
		}
	})

	t.Run("stream read failure is surfaced", func(t *testing.T) {
		readErr := errors.New("stream body read failed")
		client := New(
			WithAPIKey("test-key"),
			WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       errorReadCloser{err: readErr},
				}, nil
			})}),
		)
		_, errs := collect(client, req)
		if len(errs) != 1 || !errors.Is(errs[0], readErr) {
			t.Fatalf("errors = %v, want stream read failure", errs)
		}
	})
}

func TestADKModelStreamResponseContracts(t *testing.T) {
	t.Run("text chunks are followed by an assembled final response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{\"content\":\"llo\"}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer server.Close()
		model, err := NewADKModel(ModelFlash, New(WithBaseURLForTest(server.URL), WithAPIKey("test-key")))
		if err != nil {
			t.Fatal(err)
		}
		var got []*adkmodel.LLMResponse
		for resp, err := range model.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			got = append(got, resp)
		}
		if len(got) != 3 || !got[0].Partial || !got[1].Partial || got[2].Partial || !got[2].TurnComplete {
			t.Fatalf("responses = %+v, want two partials and one complete response", got)
		}
		if got[2].Content.Parts[0].Text != "hello" || got[2].ModelVersion != ModelFlash || got[2].FinishReason != genai.FinishReason("stop") {
			t.Fatalf("final response = %+v", got[2])
		}
	})

	t.Run("tool-call fragments are assembled into one ADK call", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"command_\",\"arguments\":\"{\\\"com\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"run\",\"arguments\":\"mand\\\":\\\"pwd\\\"}\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer server.Close()
		model, err := NewADKModel(ModelFlash, New(WithBaseURLForTest(server.URL), WithAPIKey("test-key")))
		if err != nil {
			t.Fatal(err)
		}
		var got *adkmodel.LLMResponse
		for resp, err := range model.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			got = resp
		}
		if got == nil || len(got.Content.Parts) != 1 || got.Content.Parts[0].FunctionCall == nil {
			t.Fatalf("response = %+v, want assembled function call", got)
		}
		call := got.Content.Parts[0].FunctionCall
		if call.ID != "call-1" || call.Name != "command_run" || call.Args["command"] != "pwd" {
			t.Fatalf("function call = %+v", call)
		}
	})

	t.Run("empty stream still completes the turn", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer server.Close()
		model, err := NewADKModel(ModelFlash, New(WithBaseURLForTest(server.URL), WithAPIKey("test-key")))
		if err != nil {
			t.Fatal(err)
		}
		var got []*adkmodel.LLMResponse
		for resp, err := range model.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
			if err != nil {
				t.Fatalf("generate: %v", err)
			}
			got = append(got, resp)
		}
		if len(got) != 1 || !got[0].TurnComplete || got[0].Partial {
			t.Fatalf("responses = %+v, want one completed empty turn", got)
		}
	})

	t.Run("provider error reaches ADK caller", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("maintenance"))
		}))
		defer server.Close()
		model, err := NewADKModel(ModelFlash, New(WithBaseURLForTest(server.URL), WithAPIKey("test-key")))
		if err != nil {
			t.Fatal(err)
		}
		var gotErr error
		for _, err := range model.GenerateContent(context.Background(), &adkmodel.LLMRequest{}, true) {
			if err != nil {
				gotErr = err
			}
		}
		if gotErr == nil || !strings.Contains(gotErr.Error(), "status=503") {
			t.Fatalf("error = %v, want provider error", gotErr)
		}
	})
}

func TestADKModelConfigurationContracts(t *testing.T) {
	if _, err := NewADKModel("deepseek-unknown", New(WithAPIKey("test-key"))); !errors.Is(err, ErrUnsupportedModel) {
		t.Fatalf("error = %v, want unsupported model", err)
	}
	model, err := NewADKModelWithThinking(ModelPro, New(WithAPIKey("test-key")), Thinking{Type: " "})
	if err != nil {
		t.Fatal(err)
	}
	if model.Name() != ModelPro || model.thinking == nil || model.thinking.Type != "disabled" {
		t.Fatalf("model = %+v, want model name and default thinking mode", model)
	}
}

func TestClientStreamProviderErrorBodyReadFailureIsTraced(t *testing.T) {
	readErr := errors.New("error body read failed")
	client := New(
		WithAPIKey("test-key"),
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     make(http.Header),
				Body:       errorReadCloser{err: readErr},
			}, nil
		})}),
	)
	var traces []RawTrace
	ctx := WithRawTraceRecorder(context.Background(), func(_ context.Context, trace RawTrace) {
		traces = append(traces, trace)
	})
	var gotErr error
	client.Stream(ctx, ChatRequest{Model: ModelFlash}, func(_ ChatResponse, err error) bool {
		gotErr = err
		return true
	})
	if gotErr == nil || !strings.Contains(gotErr.Error(), "status=502") {
		t.Fatalf("error = %v, want provider status error", gotErr)
	}
	var tracedReadFailure bool
	for _, trace := range traces {
		if trace.Event == "response_error" && trace.Error == readErr.Error() {
			tracedReadFailure = true
		}
	}
	if !tracedReadFailure {
		t.Fatalf("traces = %+v, want provider error-body read failure", traces)
	}
}
