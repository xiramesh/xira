package deepseek

import (
	"context"
	"os"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	"google.golang.org/adk/runner"
	adksession "google.golang.org/adk/session"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

func TestLiveADKRunnerReasoningContentToolRoundTrip(t *testing.T) {
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek reasoning_content contract test")
	}
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		t.Skip("set DEEPSEEK_API_KEY to run live DeepSeek reasoning_content contract test")
	}

	model, err := NewADKModelWithThinking(
		ModelFlash,
		New(WithAPIKey(apiKey)),
		Thinking{Type: "enabled"},
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := functiontool.New(
		functiontool.Config{Name: "lookup", Description: "Return the requested live-test value."},
		func(_ adktool.Context, args reasoningLookupArgs) (map[string]any, error) {
			return map[string]any{"query": args.Query, "value": "LIVE_REASONING_TOOL_RESULT"}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := llmagent.New(llmagent.Config{
		Name:        "live_reasoning_contract_agent",
		Description: "Exercise the DeepSeek reasoning_content tool-call round trip.",
		Instruction: "Call lookup exactly once with query alpha. After the tool result, reply with exactly LIVE_REASONING_OK and do not call any more tools.",
		Model:       model,
		Tools:       []adktool.Tool{lookup},
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := runner.New(runner.Config{
		AppName:           "live_reasoning_contract",
		Agent:             agent,
		SessionService:    adksession.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	var sawReasoningToolCall bool
	var sawToolResult bool
	var finalText string
	for event, err := range run.Run(
		context.Background(),
		"live-user",
		"live-reasoning-session",
		genai.NewContentFromText("Run the live reasoning-content contract now.", genai.RoleUser),
		adkagent.RunConfig{StreamingMode: adkagent.StreamingModeSSE},
	) {
		if err != nil {
			t.Fatalf("live reasoning_content run: %v", err)
		}
		if event == nil || event.Content == nil {
			continue
		}
		for _, part := range event.Content.Parts {
			if part == nil {
				continue
			}
			if part.FunctionCall != nil && part.FunctionCall.Name == "lookup" && len(part.ThoughtSignature) > 0 {
				sawReasoningToolCall = true
			}
			if part.FunctionResponse != nil && part.FunctionResponse.Name == "lookup" {
				sawToolResult = true
			}
			if !part.Thought {
				finalText += part.Text
			}
		}
	}
	if !sawReasoningToolCall {
		t.Fatal("live DeepSeek tool call did not carry opaque reasoning context")
	}
	if !sawToolResult {
		t.Fatal("live ADK runner did not execute the lookup tool")
	}
	if !strings.Contains(finalText, "LIVE_REASONING_OK") {
		t.Fatalf("live final text = %q, want LIVE_REASONING_OK", finalText)
	}
}
