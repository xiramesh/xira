package agents

import (
	"strings"
	"testing"
)

func TestBuiltinDefaultAssistantValidates(t *testing.T) {
	profile := BuiltinXiraAssistant()
	if err := profile.Validate(); err != nil {
		t.Fatalf("builtin profile should validate: %v", err)
	}
	if profile.ID != DefaultAgentID {
		t.Fatalf("default profile id = %q, want %q", profile.ID, DefaultAgentID)
	}
}

func TestBuiltinResearchAssistantValidates(t *testing.T) {
	profile := BuiltinResearchAssistant()
	if err := profile.Validate(); err != nil {
		t.Fatalf("builtin profile should validate: %v", err)
	}
	if profile.ID != ResearchAssistantAgentID {
		t.Fatalf("research profile id = %q, want %q", profile.ID, ResearchAssistantAgentID)
	}
}

func TestBuiltinManagerListsProfilesWithoutDuplicatingToolStrategy(t *testing.T) {
	manager, err := NewBuiltinManager()
	if err != nil {
		t.Fatal(err)
	}
	profiles := manager.List()
	if len(profiles) != 2 || profiles[0].ID != ResearchAssistantAgentID || profiles[1].ID != DefaultAgentID {
		t.Fatalf("builtin profiles = %+v, want sorted research and default profiles", profiles)
	}
	for _, profile := range profiles {
		instructions := profile.InstructionText()
		for _, toolName := range []string{"command.run", "shell.run", "tool_output.read"} {
			if strings.Contains(instructions, toolName) {
				t.Errorf("profile %s duplicates strategy for %s:\n%s", profile.ID, toolName, instructions)
			}
		}
	}
}

func TestNewManagerRejectsInvalidProfileWithIdentity(t *testing.T) {
	_, err := NewManager([]Profile{{ID: "broken-agent"}})
	if err == nil || !strings.Contains(err.Error(), `invalid profile "broken-agent"`) {
		t.Fatalf("NewManager() error = %v, want invalid profile identity", err)
	}
}

func TestProfileValidateRequiresBoundaries(t *testing.T) {
	profile := Profile{ID: "x", Name: "X", Version: "0.1.1"}
	if err := profile.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestProfileValidateModelFormat(t *testing.T) {
	for _, format := range []string{"", "text", "json", "JSON", " Json "} {
		profile := BuiltinXiraAssistant()
		profile.ModelPolicy.Format = format
		if err := profile.Validate(); err != nil {
			t.Errorf("format %q should validate: %v", format, err)
		}
	}

	profile := BuiltinXiraAssistant()
	profile.ModelPolicy.Format = "jsno"
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), `model_policy.format must be "text" or "json"`) {
		t.Fatalf("Validate() error = %v, want rejected model_policy.format", err)
	}
}

func TestModelPolicyNormalizedFormatDefaultsToText(t *testing.T) {
	tests := []struct {
		name   string
		format string
		want   string
	}{
		{name: "omitted", want: "text"},
		{name: "explicit text", format: " TEXT ", want: "text"},
		{name: "json", format: " Json ", want: "json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := ModelPolicy{Format: tt.format}
			if got := policy.NormalizedFormat(); got != tt.want {
				t.Fatalf("NormalizedFormat() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDelegationPolicyDefaultsDisabled(t *testing.T) {
	profile := BuiltinResearchAssistant()
	profile.Delegation = DelegationPolicy{}

	policy := profile.NormalizedDelegationPolicy()
	if policy.Enabled {
		t.Fatalf("delegation should default disabled: %+v", policy)
	}
	if len(policy.Allow) != 0 {
		t.Fatalf("delegation allow = %+v, want empty", policy.Allow)
	}
	if policy.MaxDepth != 1 || policy.MaxParallel != 1 || policy.MaxOutstanding != 4 {
		t.Fatalf("delegation limits = depth %d parallel %d outstanding %d", policy.MaxDepth, policy.MaxParallel, policy.MaxOutstanding)
	}
	if policy.DefaultMaxDurationMS != 30000 || policy.MaxDurationMS != 120000 {
		t.Fatalf("delegation duration defaults = %+v", policy)
	}
	if policy.ChildSessionMode != "ephemeral_worker" || policy.ReturnTo != "caller" {
		t.Fatalf("delegation routing defaults = %+v", policy)
	}
}

func TestBuiltinXiraAssistantCanDelegateOnlyToResearchAssistant(t *testing.T) {
	policy := BuiltinXiraAssistant().NormalizedDelegationPolicy()
	if !policy.Enabled {
		t.Fatalf("xira assistant delegation should be enabled: %+v", policy)
	}
	if !policy.Allows(ResearchAssistantAgentID) {
		t.Fatalf("xira assistant should allow %q: %+v", ResearchAssistantAgentID, policy)
	}
	if policy.Allows(DefaultAgentID) {
		t.Fatalf("xira assistant should not allow delegating to itself: %+v", policy)
	}
	if BuiltinResearchAssistant().NormalizedDelegationPolicy().Enabled {
		t.Fatal("research assistant should not be able to delegate by default")
	}
}

func TestDelegationPolicyValidationRejectsInvalidValues(t *testing.T) {
	profile := BuiltinXiraAssistant()
	profile.Delegation = DelegationPolicy{
		Enabled:          true,
		Allow:            []string{ResearchAssistantAgentID},
		MaxDepth:         -1,
		MaxParallel:      1,
		ChildSessionMode: "conversation_agent",
		ReturnTo:         "caller",
	}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "delegation.max_depth") {
		t.Fatalf("Validate() error = %v, want delegation.max_depth", err)
	}

	profile = BuiltinXiraAssistant()
	profile.Delegation = DelegationPolicy{
		Enabled:          true,
		Allow:            []string{ResearchAssistantAgentID},
		MaxDepth:         2,
		MaxParallel:      1,
		ChildSessionMode: "ephemeral_worker",
		ReturnTo:         "caller",
	}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "delegation.max_depth") {
		t.Fatalf("Validate() error = %v, want delegation.max_depth", err)
	}

	profile = BuiltinXiraAssistant()
	profile.Delegation = DelegationPolicy{
		Enabled:          true,
		Allow:            []string{ResearchAssistantAgentID},
		MaxDepth:         1,
		MaxParallel:      1,
		ChildSessionMode: "conversation_agent",
		ReturnTo:         "caller",
	}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "delegation.child_session_mode") {
		t.Fatalf("Validate() error = %v, want delegation.child_session_mode", err)
	}

	profile = BuiltinXiraAssistant()
	profile.Delegation = DelegationPolicy{
		Enabled:          true,
		Allow:            []string{ResearchAssistantAgentID},
		MaxDepth:         1,
		MaxParallel:      1,
		MaxOutstanding:   -1,
		ChildSessionMode: "ephemeral_worker",
		ReturnTo:         "caller",
	}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "delegation.max_outstanding") {
		t.Fatalf("Validate() error = %v, want delegation.max_outstanding", err)
	}
}
