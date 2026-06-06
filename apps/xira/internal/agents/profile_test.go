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

func TestProfileValidateRequiresBoundaries(t *testing.T) {
	profile := Profile{ID: "x", Name: "X", Version: "0.1.1"}
	if err := profile.Validate(); err == nil {
		t.Fatal("expected validation error")
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
	if policy.MaxDepth != 1 || policy.MaxParallel != 1 {
		t.Fatalf("delegation limits = depth %d parallel %d", policy.MaxDepth, policy.MaxParallel)
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
}
