package agents

import (
	"errors"
	"fmt"
	"strings"
)

const (
	DefaultAgentID           = "xira-assistant"
	ResearchAssistantAgentID = "research-assistant"
)

type Profile struct {
	ID           string             `json:"id" yaml:"id"`
	Name         string             `json:"name" yaml:"name"`
	Version      string             `json:"version" yaml:"version"`
	Description  string             `json:"description,omitempty" yaml:"description,omitempty"`
	ModelPolicy  ModelPolicy        `json:"model_policy" yaml:"model_policy"`
	Instructions []string           `json:"instructions" yaml:"instructions"`
	Context      ContextPolicy      `json:"context,omitempty" yaml:"context,omitempty"`
	Skills       []string           `json:"skills,omitempty" yaml:"skills,omitempty"`
	MCPServers   []string           `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`
	Session      SessionPolicy      `json:"session,omitempty" yaml:"session,omitempty"`
	Permissions  Permissions        `json:"permissions" yaml:"permissions"`
	Verification VerificationPolicy `json:"verification,omitempty" yaml:"verification,omitempty"`
	Artifacts    ArtifactPolicy     `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
	Evolution    EvolutionPolicy    `json:"evolution,omitempty" yaml:"evolution,omitempty"`
}

type ModelPolicy struct {
	Provider string              `json:"provider" yaml:"provider"`
	Model    string              `json:"model" yaml:"model"`
	Stream   bool                `json:"stream,omitempty" yaml:"stream,omitempty"`
	Temp     *float32            `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	Thinking ModelThinkingPolicy `json:"thinking,omitempty" yaml:"thinking,omitempty"`
}

type ModelThinkingPolicy struct {
	Type string `json:"type,omitempty" yaml:"type,omitempty"`
}

type ContextPolicy struct {
	Required  []string `json:"required,omitempty" yaml:"required,omitempty"`
	Optional  []string `json:"optional,omitempty" yaml:"optional,omitempty"`
	Forbidden []string `json:"forbidden,omitempty" yaml:"forbidden,omitempty"`
}

type SessionPolicy struct {
	Dimensions    []string            `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	IdentityLinks map[string][]string `json:"identity_links,omitempty" yaml:"identity_links,omitempty"`
}

type Permissions struct {
	Tools   []string `json:"tools" yaml:"tools"`
	Secrets []string `json:"secrets,omitempty" yaml:"secrets,omitempty"`
}

type VerificationPolicy struct {
	DefaultChecks []string `json:"default_checks,omitempty" yaml:"default_checks,omitempty"`
}

type ArtifactPolicy struct {
	OutputDir string `json:"output_dir,omitempty" yaml:"output_dir,omitempty"`
	Retention string `json:"retention,omitempty" yaml:"retention,omitempty"`
}

type EvolutionPolicy struct {
	Enabled       bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	CandidateOnly bool `json:"candidate_only,omitempty" yaml:"candidate_only,omitempty"`
}

func BuiltinProfiles() []Profile {
	return []Profile{BuiltinXiraAssistant(), BuiltinResearchAssistant()}
}

func BuiltinXiraAssistant() Profile {
	return Profile{
		ID:          DefaultAgentID,
		Name:        "Xira Assistant",
		Version:     "0.1.2",
		Description: "Default Xira runtime assistant for channel entrypoints and operational guidance.",
		ModelPolicy: ModelPolicy{
			Provider: "deepseek",
			Model:    "deepseek-v4-flash",
			Stream:   true,
			Temp:     float32Ptr(0.2),
			Thinking: ModelThinkingPolicy{Type: "disabled"},
		},
		Instructions: []string{
			"You are Xira's default runtime assistant.",
			"Reply directly to the user in the user's language.",
			"Do not pretend a specialized agent or flow is active unless the user explicitly invokes one.",
			"When useful, mention the exact Xira command the user can run, such as /agents, /agent <id> <message>, /use <id>, or /flows.",
			"Use command.run by default for local commands. Use shell.run only when shell language is required, such as pipes, redirection, &&, command substitution, or heredocs.",
			"When command output is truncated and the missing content matters, use tool_output.read with raw_output_path to read a bounded stdout or stderr slice before drawing conclusions.",
			"Keep answers concise and operational.",
		},
		Permissions:  Permissions{Tools: BuiltinToolNames()},
		Verification: VerificationPolicy{DefaultChecks: []string{"final_response_non_empty"}},
		Artifacts:    ArtifactPolicy{OutputDir: "artifacts", Retention: "local"},
		Evolution:    EvolutionPolicy{Enabled: true, CandidateOnly: true},
	}
}

func BuiltinResearchAssistant() Profile {
	return Profile{
		ID:          ResearchAssistantAgentID,
		Name:        "Research Assistant",
		Version:     "0.1.2",
		Description: "Local-first research assistant for evidence search, summaries, and Xira Phase 1 validation.",
		ModelPolicy: ModelPolicy{
			Provider: "deepseek",
			Model:    "deepseek-v4-flash",
			Stream:   true,
			Temp:     float32Ptr(0.2),
			Thinking: ModelThinkingPolicy{Type: "disabled"},
		},
		Instructions: []string{
			"You are Xira's built-in research assistant.",
			"Prefer local evidence and tool results over unsupported guesses.",
			"When using external commands, use command.run by default. Use shell.run only when shell language is required, such as pipes, redirection, &&, command substitution, or heredocs.",
			"When stdout_preview or stderr_preview is truncated, use tool_output.read against raw_output_path before relying on the missing part of the output; for failures, prefer stderr tail first.",
		},
		Permissions:  Permissions{Tools: BuiltinToolNames()},
		Verification: VerificationPolicy{DefaultChecks: []string{"final_response_non_empty"}},
		Artifacts:    ArtifactPolicy{OutputDir: "artifacts", Retention: "local"},
		Evolution:    EvolutionPolicy{Enabled: true, CandidateOnly: true},
	}
}

func BuiltinToolNames() []string {
	return []string{"command.run", "shell.run", "tool_output.read", "read_file", "search_file", "write_file", "list_dir", "edit_file"}
}

func float32Ptr(value float32) *float32 {
	return &value
}

func (p Profile) InstructionText() string {
	return strings.Join(p.Instructions, "\n")
}

func (p Profile) Validate() error {
	var errs []string
	if strings.TrimSpace(p.ID) == "" {
		errs = append(errs, "id is required")
	}
	if strings.TrimSpace(p.Name) == "" {
		errs = append(errs, "name is required")
	}
	if strings.TrimSpace(p.Version) == "" {
		errs = append(errs, "version is required")
	}
	if strings.TrimSpace(p.ModelPolicy.Provider) == "" {
		errs = append(errs, "model_policy.provider is required")
	}
	if strings.TrimSpace(p.ModelPolicy.Model) == "" {
		errs = append(errs, "model_policy.model is required")
	}
	if len(p.Instructions) == 0 {
		errs = append(errs, "instructions is required")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	if p.ModelPolicy.Provider != "deepseek" {
		return fmt.Errorf("unsupported provider %q", p.ModelPolicy.Provider)
	}
	return nil
}
