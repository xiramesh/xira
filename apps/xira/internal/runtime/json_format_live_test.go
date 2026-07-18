package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
	rtools "github.com/xiramesh/xira/internal/tools"
)

// TestRealDeepSeekJSONResponseFormat proves the full PROFILE.md -> runtime ->
// ADK -> DeepSeek -> validated public final chain against the real provider.
// The user message intentionally does not mention JSON: the runtime-owned
// response-format instruction must satisfy DeepSeek's JSON-mode prompt rule.
func TestRealDeepSeekJSONResponseFormat(t *testing.T) {
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek JSON format tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek JSON format tests")
	}
	model := strings.TrimSpace(os.Getenv("XIRA_DEEPSEEK_MODEL"))
	if model == "" {
		model = deepseek.ModelPro
	}
	if !deepseek.SupportedModel(model) {
		t.Fatalf("unsupported live DeepSeek model %q", model)
	}

	workspace := t.TempDir()
	stateRoot := filepath.Join(t.TempDir(), "state")
	writeFile(t, filepath.Join(workspace, "agents", "structured-assistant", "PROFILE.md"), `---
id: structured-assistant
name: Structured Assistant
version: 0.1.0
description: Live structured-response smoke assistant.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
  format: json
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

Return an object containing exactly two fields: status set to ok and count set to 2.
`)
	writeFile(t, filepath.Join(workspace, "agents", "structured-assistant", "SOUL.md"), "# Soul\n\nPrecise.\n")
	writeFile(t, filepath.Join(workspace, "xira.yaml"), `workspace: .
default_agent: structured-assistant
entrypoints: entrypoints.yaml
`)
	writeFile(t, filepath.Join(workspace, "entrypoints.yaml"), `entrypoints:
  - id: test-default
    channel: test
    default_agent: structured-assistant
`)

	cfg := Config{ConfigPath: filepath.Join(workspace, "xira.yaml"), StateDir: stateRoot}
	manager, err := NewSessionManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.SessionManager = manager
	rt, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := rt.RunAgent(context.Background(), TurnRequest{
		Message: "Give me the requested structured result.",
		Context: channel.NewInboundContext("test", "live-json-format-user", nil),
	})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status != "completed" || resp.ModelPolicy.Format != "json" {
		t.Fatalf("status/model policy = %q/%+v, want completed JSON run", resp.Status, resp.ModelPolicy)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(resp.FinalResponse), &object); err != nil {
		t.Fatalf("final response = %q, want JSON object: %v", resp.FinalResponse, err)
	}
	if len(object) != 2 || object["status"] != "ok" || object["count"] != float64(2) {
		t.Fatalf("final object = %#v, want exactly status=ok and count=2", object)
	}
	if _, ok := findEvent(resp.Events, "assistant.final"); !ok {
		t.Fatalf("completed JSON run missing assistant.final: %v", eventKinds(resp.Events))
	}
}

// TestRealDeepSeekJSONResponseFormatWithTools pins the cross-feature contract
// raised in PR #183 review: every ADK step may carry response_format and tools
// together, including the final request after a tool result. Both supported
// DeepSeek models must complete the real tool round-trip and return an object.
func TestRealDeepSeekJSONResponseFormatWithTools(t *testing.T) {
	if os.Getenv("XIRA_DEEPSEEK_LIVE") != "1" {
		t.Skip("set XIRA_DEEPSEEK_LIVE=1 to run live DeepSeek JSON tool tests")
	}
	if strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY")) == "" {
		t.Skip("DEEPSEEK_API_KEY is required for live DeepSeek JSON tool tests")
	}

	for _, model := range []string{deepseek.ModelPro, deepseek.ModelFlash} {
		t.Run(model, func(t *testing.T) {
			var wireRequests []deepseek.ChatRequest
			recordingClient := &http.Client{
				Timeout: 90 * time.Second,
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					_ = req.Body.Close()
					req.Body = io.NopCloser(bytes.NewReader(body))
					var wireRequest deepseek.ChatRequest
					if err := json.Unmarshal(body, &wireRequest); err != nil {
						return nil, err
					}
					wireRequests = append(wireRequests, wireRequest)
					return http.DefaultTransport.RoundTrip(req)
				}),
			}
			workspace := t.TempDir()
			stateRoot := filepath.Join(t.TempDir(), "state")
			writeFile(t, filepath.Join(workspace, "agents", "structured-tool-assistant", "PROFILE.md"), `---
id: structured-tool-assistant
name: Structured Tool Assistant
version: 0.1.0
description: Live structured tool-response smoke assistant.
model_policy:
  provider: deepseek
  model: `+model+`
  stream: false
  format: json
permissions:
  tools:
    - update_memory
verification:
  default_checks:
    - final_response_non_empty
---
# Working Contract

When asked, call update_memory exactly once with scope=agent, key=structured-tool-proof, and content=provider combination verified. After the tool succeeds, return an object containing exactly status set to ok and tool set to update_memory.
`)
			writeFile(t, filepath.Join(workspace, "agents", "structured-tool-assistant", "SOUL.md"), "# Soul\n\nPrecise.\n")
			writeFile(t, filepath.Join(workspace, "xira.yaml"), `workspace: .
default_agent: structured-tool-assistant
entrypoints: entrypoints.yaml
`)
			writeFile(t, filepath.Join(workspace, "entrypoints.yaml"), `entrypoints:
  - id: test-default
    channel: test
    default_agent: structured-tool-assistant
`)

			cfg := Config{
				ConfigPath: filepath.Join(workspace, "xira.yaml"),
				StateDir:   stateRoot,
				DeepSeekClient: deepseek.New(
					deepseek.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")),
					deepseek.WithHTTPClient(recordingClient),
				),
			}
			manager, err := NewSessionManager(cfg)
			if err != nil {
				t.Fatal(err)
			}
			cfg.SessionManager = manager
			rt, err := NewService(cfg)
			if err != nil {
				t.Fatal(err)
			}

			resp, err := rt.RunAgent(context.Background(), TurnRequest{
				Message: "Call update_memory exactly once with scope=agent, key=structured-tool-proof, and content=provider combination verified. Do not return a final response until the tool result is available. Then return the requested structured result.",
				Context: channel.NewInboundContext("test", "live-json-tool-user", nil),
			})
			if err != nil {
				t.Fatalf("RunAgent() error = %v", err)
			}
			if resp.Status != "completed" || resp.ModelPolicy.Format != "json" {
				t.Fatalf("status/model policy = %q/%+v, want completed JSON tool run", resp.Status, resp.ModelPolicy)
			}
			if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "update_memory" || resp.ToolCalls[0].Input["scope"] != "agent" {
				t.Fatalf("tool calls = %+v final=%q, want one agent-scoped update_memory", resp.ToolCalls, resp.FinalResponse)
			}
			if len(wireRequests) != 2 {
				t.Fatalf("wire requests = %d, want tool-call and post-tool final requests", len(wireRequests))
			}
			for i, request := range wireRequests {
				if request.ResponseFormat == nil || request.ResponseFormat.Type != "json_object" || len(request.Tools) == 0 {
					t.Fatalf("wire request %d response_format/tools = %+v/%d, want json_object with tools", i+1, request.ResponseFormat, len(request.Tools))
				}
			}
			var object map[string]any
			if err := json.Unmarshal([]byte(resp.FinalResponse), &object); err != nil {
				t.Fatalf("final response = %q, want JSON object: %v", resp.FinalResponse, err)
			}
			if len(object) != 2 || object["status"] != "ok" || object["tool"] != "update_memory" {
				t.Fatalf("final object = %#v, want exactly status=ok and tool=update_memory", object)
			}
			entries, err := rtools.LoadMemories(rtools.AgentMemoryPath(stateRoot, "structured-tool-assistant"))
			if err != nil || len(entries) != 1 || !strings.Contains(entries[0].Content, "provider combination verified") {
				t.Fatalf("agent memory = %+v, err=%v, want persisted tool result", entries, err)
			}
			if _, ok := findEvent(resp.Events, "assistant.final"); !ok {
				t.Fatalf("completed JSON tool run missing assistant.final: %v", eventKinds(resp.Events))
			}
		})
	}
}
