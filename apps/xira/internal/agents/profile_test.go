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

// TestDelegationTargetPolicyNormalize: per-target policy gets normalized (worker_mode
// trimmed/lowercased, empty targets dropped) and MaxDurationMS preserved.
func TestDelegationTargetPolicyNormalize(t *testing.T) {
	policy := DelegationPolicy{
		Enabled: true, Allow: []string{"code-agent"},
		MaxDepth: 1, MaxParallel: 1, ChildSessionMode: "ephemeral_worker", ReturnTo: "caller",
		Targets: map[string]DelegationTargetPolicy{
			"code-agent": {
				WorkerMode:    "  External_Command ",
				MaxDurationMS: 7200000,
				ExposeProgress: true,
			},
			"  ": {WorkerMode: "external_command"}, // blank key dropped
		},
	}
	normalized := NormalizeDelegationPolicy(policy)
	if len(normalized.Targets) != 1 {
		t.Fatalf("expected 1 target after normalize, got %d: %+v", len(normalized.Targets), normalized.Targets)
	}
	tp, ok := normalized.Targets["code-agent"]
	if !ok {
		t.Fatalf("code-agent target missing after normalize: %+v", normalized.Targets)
	}
	if tp.WorkerMode != "external_command" {
		t.Fatalf("worker_mode not normalized: %q", tp.WorkerMode)
	}
	if tp.MaxDurationMS != 7200000 {
		t.Fatalf("max_duration_ms not preserved: %d", tp.MaxDurationMS)
	}
	if !tp.ExposeProgress {
		t.Fatalf("expose_progress not preserved")
	}
}

// TestDelegationTargetPolicyValidate: worker_mode must be a known value; an
// unknown mode is rejected by Validate.
func TestDelegationTargetPolicyValidate(t *testing.T) {
	// Known mode passes.
	profile := BuiltinXiraAssistant()
	profile.Delegation = DelegationPolicy{
		Enabled: true, Allow: []string{"code-agent"},
		MaxDepth: 1, MaxParallel: 1, ChildSessionMode: "ephemeral_worker", ReturnTo: "caller",
		Targets: map[string]DelegationTargetPolicy{
			"code-agent": {WorkerMode: "external_command", MaxDurationMS: 7200000},
		},
	}
	if err := profile.Validate(); err != nil {
		t.Fatalf("valid worker_mode should pass, got: %v", err)
	}

	// Unknown mode fails.
	profile.Delegation.Targets["code-agent"] = DelegationTargetPolicy{WorkerMode: "bogus_mode"}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "worker_mode") {
		t.Fatalf("unknown worker_mode should fail with worker_mode mention, got: %v", err)
	}
}

// TestDelegationTargetPolicyAbsent: a target with no fields is dropped (empty
// target policy = no per-target override).
func TestDelegationTargetPolicyAbsent(t *testing.T) {
	policy := DelegationPolicy{
		Enabled: true, Allow: []string{"x"},
		MaxDepth: 1, MaxParallel: 1, ChildSessionMode: "ephemeral_worker", ReturnTo: "caller",
		Targets: map[string]DelegationTargetPolicy{"x": {}},
	}
	normalized := NormalizeDelegationPolicy(policy)
	if _, ok := normalized.Targets["x"]; ok {
		t.Fatalf("empty target policy should be dropped after normalize")
	}
}

// TestBuiltinManagerAndList: the builtin manager loads builtin profiles and
// List returns them sorted by ID. Also covers BuiltinProfiles + NewBuiltinManager.
func TestBuiltinManagerAndList(t *testing.T) {
	m, err := NewBuiltinManager()
	if err != nil {
		t.Fatalf("NewBuiltinManager: %v", err)
	}
	list := m.List()
	if len(list) < 1 {
		t.Fatalf("expected at least one builtin profile, got %d", len(list))
	}
	// Sorted by ID.
	for i := 1; i < len(list); i++ {
		if list[i-1].ID > list[i].ID {
			t.Fatalf("List not sorted by ID at %d: %q > %q", i, list[i-1].ID, list[i].ID)
		}
	}
	// Get returns the profile.
	if _, ok := m.Get(DefaultAgentID); !ok {
		t.Fatalf("Get(%q) missing", DefaultAgentID)
	}
	if _, ok := m.Get("does-not-exist"); ok {
		t.Fatalf("Get on missing id should return false")
	}
}

// TestAllowsEdgeCases: Allows rejects empty/whitespace and matches trimmed.
func TestAllowsEdgeCases(t *testing.T) {
	policy := DelegationPolicy{Allow: []string{"  code-agent  "}}
	if !policy.Allows("code-agent") {
		t.Fatalf("Allows should match trimmed entry")
	}
	if policy.Allows("") {
		t.Fatalf("Allows('') should be false")
	}
	if policy.Allows("   ") {
		t.Fatalf("Allows(whitespace) should be false")
	}
	if policy.Allows("other") {
		t.Fatalf("Allows on non-listed id should be false")
	}
}

// TestNewManagerRejectsInvalidProfile: NewManager validates each profile.
func TestNewManagerRejectsInvalidProfile(t *testing.T) {
	bad := BuiltinXiraAssistant()
	bad.ID = "" // invalid
	if _, err := NewManager([]Profile{bad}); err == nil {
		t.Fatalf("NewManager should reject invalid profile")
	}
}
