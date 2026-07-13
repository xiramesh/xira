package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type guidanceProbeTool struct {
	name     string
	guidance string
}

func (t guidanceProbeTool) Name() string               { return t.name }
func (t guidanceProbeTool) Description() string        { return "probe" }
func (t guidanceProbeTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (t guidanceProbeTool) Guidance() string           { return t.guidance }
func (t guidanceProbeTool) Execute(context.Context, map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestRegistryDefinitionCarriesOptionalToolGuidance(t *testing.T) {
	reg := NewRegistry([]Tool{
		guidanceProbeTool{name: "guided", guidance: "  Use this when durable context matters.  "},
		&ReadFileTool{},
	})

	guided, ok := reg.GetDefinition("guided")
	if !ok {
		t.Fatal("guided definition missing")
	}
	if guided.Guidance != "Use this when durable context matters." {
		t.Fatalf("guided Guidance = %q", guided.Guidance)
	}

	unguided, ok := reg.GetDefinition("read_file")
	if !ok {
		t.Fatal("read_file definition missing")
	}
	if unguided.Guidance != "" {
		t.Fatalf("read_file Guidance = %q, want empty", unguided.Guidance)
	}
}

func TestBuiltinToolsExposeGuidanceOnlyWhenSemanticSteeringIsNeeded(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistryForAgent(t.TempDir(), []string{
		"command.run", "shell.run", "tool_output.read",
		"read_file", "search_file", "write_file", "list_dir", "edit_file",
		"update_profile", "update_memory", "forget_memory",
	}, SandboxRoots{}, stateDir, "agent-a")

	wantGuidance := map[string]bool{
		"command.run":      true,
		"shell.run":        true,
		"tool_output.read": true,
		"update_profile":   true,
		"update_memory":    true,
		"forget_memory":    true,
	}
	for _, def := range reg.Definitions() {
		if got := strings.TrimSpace(def.Guidance) != ""; got != wantGuidance[def.Name] {
			t.Errorf("tool %s has Guidance = %v, want %v\n%s", def.Name, got, wantGuidance[def.Name], def.Guidance)
		}
	}
}

func TestBuiltinToolGuidanceIsSelfContained(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistryForAgent(t.TempDir(), []string{
		"command.run", "shell.run", "tool_output.read", "update_profile", "update_memory", "forget_memory",
	}, SandboxRoots{}, stateDir, "agent-a")
	toolNames := reg.List()
	for _, def := range reg.Definitions() {
		for _, other := range toolNames {
			if other == def.Name {
				continue
			}
			if strings.Contains(def.Guidance, other) {
				t.Errorf("Guidance for %s references other tool %s:\n%s", def.Name, other, def.Guidance)
			}
		}
	}
}

func TestBuiltinToolDescriptionsDoNotRequireAnotherTool(t *testing.T) {
	stateDir := t.TempDir()
	reg := NewBuiltinRegistryForAgent(t.TempDir(), []string{
		"command.run", "shell.run", "tool_output.read", "update_profile", "update_memory", "forget_memory",
	}, SandboxRoots{}, stateDir, "agent-a")
	toolNames := reg.List()
	for _, def := range reg.Definitions() {
		modelVisibleText := def.Description + "\n" + toJSONForGuidanceTest(t, def.Parameters)
		for _, other := range toolNames {
			if other == def.Name {
				continue
			}
			if strings.Contains(modelVisibleText, other) {
				t.Errorf("model-visible definition for %s requires other tool %s:\n%s", def.Name, other, modelVisibleText)
			}
		}
	}
}

func TestProfileAndMemoryGuidanceDeclareDisjointSubjects(t *testing.T) {
	profileGuidance := NewUpdateProfileTool(t.TempDir()).Guidance()
	for _, want := range []string{"identity and interaction-profile facts", "do not use it for episodic events"} {
		if !strings.Contains(profileGuidance, want) {
			t.Errorf("profile Guidance missing subject boundary %q:\n%s", want, profileGuidance)
		}
	}
	memoryGuidance := NewUpdateMemoryToolForAgent(t.TempDir(), "agent-a").Guidance()
	for _, want := range []string{"durable episodic context", "do not belong in sender memory"} {
		if !strings.Contains(memoryGuidance, want) {
			t.Errorf("memory Guidance missing subject boundary %q:\n%s", want, memoryGuidance)
		}
	}
}

func toJSONForGuidanceTest(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
