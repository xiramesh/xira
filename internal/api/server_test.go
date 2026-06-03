package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ai-daming/flowdeck/internal/agents"
	frt "github.com/ai-daming/flowdeck/internal/runtime"
)

func TestAgentRunAPI(t *testing.T) {
	rt, err := frt.NewService(frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(frt.TurnRequest{Message: "hello", Channel: "test"})
	resp, err := http.Post(server.URL()+"/api/v1/agent-runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var run frt.TurnResponse
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if run.RunID == "" || run.FinalResponse == "" {
		t.Fatalf("bad run response: %+v", run)
	}
}

func TestShellTurnAPIIsNotAChannelEntrypoint(t *testing.T) {
	rt, err := frt.NewService(frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(frt.TurnRequest{Message: "hi", Channel: "test"})
	resp, err := http.Post(server.URL()+"/api/v1/shell-turns", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAgentsAPIUsesWorkspaceDiscoveredAgents(t *testing.T) {
	workspace := writeAPIWorkspace(t)
	rt, err := frt.NewService(frt.Config{
		WorkspaceRoot:  workspace,
		DefaultAgentID: "flowdeck-assistant",
		RunRoot:        filepath.Join(t.TempDir(), "runs"),
		UseMockModel:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(server.URL() + "/api/v1/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var profiles []agents.Profile
	if err := json.NewDecoder(resp.Body).Decode(&profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles len = %d", len(profiles))
	}
	if profiles[0].ID != "flowdeck-assistant" || profiles[1].ID != "research-assistant" {
		t.Fatalf("profiles = %+v", profiles)
	}
}

func TestEventsWebSocketReceivesRunEvents(t *testing.T) {
	rt, err := frt.NewService(frt.Config{RunRoot: filepath.Join(t.TempDir(), "runs"), UseMockModel: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := NewServer(rt, "127.0.0.1:0")
	if err := server.StartAsync(ctx); err != nil {
		t.Fatal(err)
	}
	wsURL := "ws" + server.URL()[4:] + "/api/v1/events"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	body, _ := json.Marshal(frt.TurnRequest{Message: "hello", Channel: "test"})
	resp, err := http.Post(server.URL()+"/api/v1/agent-runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var evt frt.RuntimeEvent
	if err := conn.ReadJSON(&evt); err != nil {
		t.Fatal(err)
	}
	if evt.Kind != "run.started" {
		t.Fatalf("event kind = %q", evt.Kind)
	}
}

func writeAPIWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	writeAPIFile(t, filepath.Join(workspace, "agents", "flowdeck-assistant", "PROFILE.md"), `---
id: flowdeck-assistant
name: FlowDeck Assistant
version: 0.1.0
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
---
Default contract.
`)
	writeAPIFile(t, filepath.Join(workspace, "agents", "flowdeck-assistant", "SOUL.md"), `Default soul.`)
	writeAPIFile(t, filepath.Join(workspace, "agents", "research-assistant", "PROFILE.md"), `---
id: research-assistant
name: Research Assistant
version: 0.1.0
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
tools:
  - exec
  - read_file
  - write_file
  - list_dir
  - edit_file
---
Research contract.
`)
	writeAPIFile(t, filepath.Join(workspace, "agents", "research-assistant", "SOUL.md"), `Research soul.`)
	return workspace
}

func writeAPIFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}
