package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromWorkspaceReadsProfileFrontmatterBodyAndSoul(t *testing.T) {
	workspace := t.TempDir()
	writeAgentProfile(t, workspace, "research-assistant", `---
id: research-assistant
name: Research Assistant
version: 0.1.1
description: Evidence-first research agent.
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
  stream: true
  temperature: 0.2
tools:
  - command.run
  - shell.run
  - tool_output.read
  - search_file
  - read_file
  - write_file
  - list_dir
  - edit_file
skills:
  - local-search
mcp_servers:
  - filesystem
session:
  dimensions:
    - chat
    - sender
    - channel
verification:
  default_checks:
    - final_response_non_empty
artifacts:
  output_dir: artifacts
  retention: local
evolution:
  enabled: true
  candidate_only: true
---
# Working Contract

Use local evidence before summaries.
`, `# Research Soul

Direct, careful, and source-backed.`)

	manager, err := LoadFromWorkspace(workspace)
	if err != nil {
		t.Fatalf("LoadFromWorkspace() error = %v", err)
	}
	profile, ok := manager.Get("research-assistant")
	if !ok {
		t.Fatal("expected research-assistant profile")
	}
	if profile.Name != "Research Assistant" {
		t.Fatalf("Name = %q", profile.Name)
	}
	if got := profile.InstructionText(); !strings.Contains(got, "Use local evidence before summaries.") || !strings.Contains(got, "Direct, careful, and source-backed.") {
		t.Fatalf("InstructionText() did not include PROFILE body and SOUL.md:\n%s", got)
	}
	if got := strings.Join(profile.Permissions.Tools, ","); got != "command.run,shell.run,tool_output.read,search_file,read_file,write_file,list_dir,edit_file" {
		t.Fatalf("Permissions.Tools = %q", got)
	}
	if got := strings.Join(profile.Session.Dimensions, ","); got != "chat,sender,channel" {
		t.Fatalf("Session.Dimensions = %q", got)
	}
	if got := strings.Join(profile.MCPServers, ","); got != "filesystem" {
		t.Fatalf("MCPServers = %q", got)
	}
}

func TestLoadProfileDirRequiresIDToMatchDirectory(t *testing.T) {
	workspace := t.TempDir()
	writeAgentProfile(t, workspace, "copied-agent", `---
id: original-agent
name: Copied Agent
version: 0.1.1
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
---
Body.
`, "")

	_, err := LoadFromWorkspace(workspace)
	if err == nil {
		t.Fatal("expected id mismatch error")
	}
	if !strings.Contains(err.Error(), `PROFILE.md id "original-agent" must match agent directory "copied-agent"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadProfileDirReportsMissingRequiredFields(t *testing.T) {
	workspace := t.TempDir()
	writeAgentProfile(t, workspace, "broken-agent", `---
id: broken-agent
name: Broken Agent
version: 0.1.1
---
Body.
`, "Soul.")

	_, err := LoadFromWorkspace(workspace)
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "model_policy.provider is required") || !strings.Contains(err.Error(), "model_policy.model is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoadProfileDirRequiresSoulFile(t *testing.T) {
	workspace := t.TempDir()
	writeAgentProfile(t, workspace, "soulless-agent", `---
id: soulless-agent
name: Soulless Agent
version: 0.1.1
model_policy:
  provider: deepseek
  model: deepseek-v4-flash
---
Body.
`, "")

	_, err := LoadFromWorkspace(workspace)
	if err == nil {
		t.Fatal("expected missing SOUL.md error")
	}
	if !strings.Contains(err.Error(), "SOUL.md") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRepositoryWorkspaceDamingAgentCanExecuteFiles(t *testing.T) {
	workspace := filepath.Clean("../../../../workspace")
	manager, err := LoadFromWorkspace(workspace)
	if err != nil {
		t.Fatalf("LoadFromWorkspace(%s) error = %v", workspace, err)
	}
	profile, ok := manager.Get("daming-agent")
	if !ok {
		t.Fatal("expected daming-agent profile")
	}

	for _, tool := range []string{"command.run", "shell.run", "tool_output.read", "read_file", "search_file", "write_file", "list_dir", "edit_file"} {
		if !containsString(profile.Permissions.Tools, tool) {
			t.Fatalf("daming-agent tools = %+v, missing %q", profile.Permissions.Tools, tool)
		}
	}

	instructions := profile.InstructionText()
	for _, want := range []string{"execute commands", "command.run", "shell.run", "tool_output.read", "stdout_preview", "timeout_seconds"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("daming-agent instructions missing %q:\n%s", want, instructions)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func writeAgentProfile(t *testing.T, workspace, id, profile, soul string) {
	t.Helper()
	dir := filepath.Join(workspace, "agents", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PROFILE.md"), []byte(profile), 0o644); err != nil {
		t.Fatalf("WriteFile(PROFILE.md) error = %v", err)
	}
	if soul != "" {
		if err := os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte(soul), 0o644); err != nil {
			t.Fatalf("WriteFile(SOUL.md) error = %v", err)
		}
	}
}
