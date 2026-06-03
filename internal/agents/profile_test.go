package agents

import "testing"

func TestBuiltinDefaultAssistantValidates(t *testing.T) {
	profile := BuiltinFlowDeckAssistant()
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
	profile := Profile{ID: "x", Name: "X", Version: "0.1.0"}
	if err := profile.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}
