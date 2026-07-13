package runtime

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

func TestEffectiveToolNamesMatchActualADKToolSet(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	profile := agents.BuiltinXiraAssistant()
	ctx := context.Background()

	tools, err := rt.adkTools(ctx, profile,
		func(string, string, string, map[string]any) {},
		func(string, string, bool, string, map[string]any) {},
		func(ToolCallRecord) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	var actual []string
	for _, tool := range tools {
		actual = append(actual, tool.Name())
	}
	sort.Strings(actual)

	if effective := rt.effectiveToolNames(ctx, profile); !reflect.DeepEqual(effective, actual) {
		t.Fatalf("effective names drifted from ADK tools\neffective=%v\nactual=%v", effective, actual)
	}
}

func TestEffectiveToolNamesHonorExactRuntimeContext(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	profile := agents.BuiltinXiraAssistant()

	onlySilent := contextWithRuntimeToolAllowlist(context.Background(), []string{finishSilentToolName})
	if got := rt.effectiveToolNames(onlySilent, profile); !reflect.DeepEqual(got, []string{finishSilentToolName}) {
		t.Fatalf("allowlisted effective names = %v, want [%s]", got, finishSilentToolName)
	}

	noRuntime := contextWithRuntimeNativeToolsDisabled(context.Background())
	profile.Permissions.Tools = []string{"read_file"}
	if got := rt.effectiveToolNames(noRuntime, profile); !reflect.DeepEqual(got, []string{"read_file"}) {
		t.Fatalf("native-disabled effective names = %v, want [read_file]", got)
	}

	profile.Delegation.Enabled = false
	got := rt.effectiveToolNames(context.Background(), profile)
	for _, forbidden := range []string{spawnTurnToolName, pollTurnToolName, answerChildToolName} {
		if containsToolName(got, forbidden) {
			t.Errorf("delegation-disabled effective names contain %s: %v", forbidden, got)
		}
	}
}

func TestInstructionInjectsOnlyTheEffectiveToolsOwnGuidance(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	profile := agents.BuiltinXiraAssistant()
	profile.Permissions.Tools = []string{"update_memory"}
	ctx := contextWithRuntimeNativeToolsDisabled(context.Background())

	instruction, _, err := rt.instructionTextForRunContext(ctx, profile, channel.NewInboundContext("test", "sender-a", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Tool Guidance",
		"## update_memory",
		"without a successful call",
		"scope=sender",
		"scope=agent",
	} {
		if !strings.Contains(instruction, want) {
			t.Errorf("instruction missing %q:\n%s", want, instruction)
		}
	}
	for _, absent := range []string{
		"## update_profile", "## forget_memory", "## finish_silent", "## notify_owner",
		"command.run", "shell.run", "tool_output.read",
	} {
		if strings.Contains(instruction, absent) {
			t.Errorf("instruction contains unavailable guidance %q:\n%s", absent, instruction)
		}
	}
}

func TestInstructionOmitsGuidanceSectionWhenEffectiveToolsNeedNone(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	profile := agents.BuiltinResearchAssistant()
	profile.Permissions.Tools = []string{"read_file", "search_file"}
	ctx := contextWithRuntimeNativeToolsDisabled(context.Background())

	instruction, _, err := rt.instructionTextForRunContext(ctx, profile, channel.NewInboundContext("test", "sender-a", nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(instruction, "# Tool Guidance") {
		t.Fatalf("instruction contains empty/unneeded guidance section:\n%s", instruction)
	}
}

func TestInstructionCapabilitiesAndGuidanceShareTheExactEffectiveToolSet(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	profile := agents.BuiltinXiraAssistant()
	ctx := contextWithRuntimeToolAllowlist(context.Background(), []string{finishSilentToolName})

	instruction, _, err := rt.instructionTextForRunContext(ctx, profile, channel.NewInboundContext("test", "sender-a", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(instruction, "Available tools: finish_silent.") {
		t.Fatalf("capability list does not match exact allowlist:\n%s", instruction)
	}
	if !strings.Contains(instruction, "## finish_silent") {
		t.Fatalf("matching Guidance missing:\n%s", instruction)
	}
	for _, unavailable := range []string{"command.run", "read_file", "update_memory", "notify_owner"} {
		if strings.Contains(instruction, unavailable) {
			t.Errorf("instruction leaks unavailable tool %s:\n%s", unavailable, instruction)
		}
	}
}

func TestRunAgentSendsOnlyAllowedToolAndItsGuidanceToModel(t *testing.T) {
	var captured deepseek.ChatRequest
	rt, err := NewService(withTestSessionManager(t, Config{
		StateDir:       t.TempDir(),
		DeepSeekClient: capturingClient(t, &captured),
	}))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := rt.RunAgent(context.Background(), TurnRequest{
		Message:         "remember my future trip",
		Context:         channel.NewInboundContext("test", "sender-a", nil),
		AllowedToolsSet: true,
		AllowedTools:    []string{"update_memory"},
	}); err != nil {
		t.Fatal(err)
	}

	if len(captured.Tools) != 1 || captured.Tools[0].Function.Name != "update_memory" {
		t.Fatalf("model-visible tools = %+v, want only update_memory", captured.Tools)
	}
	definitionJSON, err := json.Marshal(captured.Tools[0].Function)
	if err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []string{"update_profile", "forget_memory"} {
		if strings.Contains(string(definitionJSON), unavailable) {
			t.Errorf("model-visible update_memory definition references unavailable tool %s:\n%s", unavailable, definitionJSON)
		}
	}
	if len(captured.Messages) == 0 || captured.Messages[0].Role != "system" {
		t.Fatalf("captured messages = %+v, want leading system instruction", captured.Messages)
	}
	instruction, ok := captured.Messages[0].Content.(string)
	if !ok {
		t.Fatalf("system instruction content = %T, want string", captured.Messages[0].Content)
	}
	for _, want := range []string{"Available tools: update_memory.", "## update_memory", "scope=sender", "scope=agent"} {
		if !strings.Contains(instruction, want) {
			t.Errorf("system instruction missing %q:\n%s", want, instruction)
		}
	}
	for _, unavailable := range []string{"command.run", "read_file", "finish_silent", "notify_owner"} {
		if strings.Contains(instruction, unavailable) {
			t.Errorf("system instruction leaks unavailable tool %s:\n%s", unavailable, instruction)
		}
	}
}

func TestToolGuidanceIsOneSelfContainedFragmentPerTool(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	profile := agents.BuiltinXiraAssistant()
	allNames := rt.effectiveToolNames(context.Background(), profile)
	wantRuntimeGuidance := map[string]bool{
		"human.request":        true,
		notifyOwnerToolName:    true,
		finishSilentToolName:   true,
		humanInterpretToolName: true,
		statusToolName:         true,
		spawnTurnToolName:      true,
		pollTurnToolName:       true,
		answerChildToolName:    true,
	}

	for _, name := range allNames {
		guidance := rt.toolGuidance(profile, name)
		if wantRuntimeGuidance[name] && strings.TrimSpace(guidance) == "" {
			t.Errorf("runtime tool %s has no Guidance", name)
		}
		if strings.TrimSpace(guidance) == "" {
			continue
		}
		for _, other := range allNames {
			if other != name && strings.Contains(guidance, other) {
				t.Errorf("Guidance for %s references other tool %s:\n%s", name, other, guidance)
			}
		}
	}
}

func TestRuntimeToolDescriptionsDoNotRequireAnotherTool(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	profile := agents.BuiltinXiraAssistant()
	adkTools, err := rt.adkTools(context.Background(), profile,
		func(string, string, string, map[string]any) {},
		func(string, string, bool, string, map[string]any) {},
		func(ToolCallRecord) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range adkTools {
		names = append(names, tool.Name())
	}
	for _, tool := range adkTools {
		for _, other := range names {
			if other == tool.Name() {
				continue
			}
			if strings.Contains(tool.Description(), other) {
				t.Errorf("Description for %s requires other tool %s:\n%s", tool.Name(), other, tool.Description())
			}
		}
		if strings.Contains(tool.Description(), "delegate_agent") {
			t.Errorf("Description for %s names retired tool delegate_agent:\n%s", tool.Name(), tool.Description())
		}
	}
}

func TestCompileToolGuidanceIsStableAndDeduplicated(t *testing.T) {
	rt := newTestService(t, Config{StateDir: t.TempDir()})
	profile := agents.BuiltinXiraAssistant()
	block := rt.compileToolGuidance(profile, []string{"update_memory", finishSilentToolName, "update_memory"})

	if strings.Count(block, "## update_memory") != 1 || strings.Count(block, "## "+finishSilentToolName) != 1 {
		t.Fatalf("compiled Guidance is not deduplicated:\n%s", block)
	}
	if strings.Index(block, "## "+finishSilentToolName) > strings.Index(block, "## update_memory") {
		t.Fatalf("compiled Guidance order is not stable lexical order:\n%s", block)
	}
}

func containsToolName(names []string, target string) bool {
	for _, name := range names {
		if name == target {
			return true
		}
	}
	return false
}
