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
	Knowledge    KnowledgePolicy    `json:"knowledge,omitempty" yaml:"knowledge,omitempty"`
	Skills       []string           `json:"skills,omitempty" yaml:"skills,omitempty"`
	MCPServers   []string           `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`
	Session      SessionPolicy      `json:"session,omitempty" yaml:"session,omitempty"`
	Permissions  Permissions        `json:"permissions" yaml:"permissions"`
	Verification VerificationPolicy `json:"verification,omitempty" yaml:"verification,omitempty"`
	Artifacts    ArtifactPolicy     `json:"artifacts,omitempty" yaml:"artifacts,omitempty"`
	Evolution    EvolutionPolicy    `json:"evolution,omitempty" yaml:"evolution,omitempty"`
}

type ModelPolicy struct {
	Provider string  `json:"provider" yaml:"provider"`
	Model    string  `json:"model" yaml:"model"`
	Stream   bool    `json:"stream,omitempty" yaml:"stream,omitempty"`
	Temp     float32 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
}

type ContextPolicy struct {
	Required  []string `json:"required,omitempty" yaml:"required,omitempty"`
	Optional  []string `json:"optional,omitempty" yaml:"optional,omitempty"`
	Forbidden []string `json:"forbidden,omitempty" yaml:"forbidden,omitempty"`
}

type KnowledgePolicy struct {
	Root    string          `json:"root,omitempty" yaml:"root,omitempty"`
	Default []string        `json:"default,omitempty" yaml:"default,omitempty"`
	Rules   []KnowledgeRule `json:"rules,omitempty" yaml:"rules,omitempty"`
}

type KnowledgeRule struct {
	ID       string   `json:"id,omitempty" yaml:"id,omitempty"`
	When     string   `json:"when,omitempty" yaml:"when,omitempty"`
	Keywords []string `json:"keywords,omitempty" yaml:"keywords,omitempty"`
	Required []string `json:"required,omitempty" yaml:"required,omitempty"`
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
		Version:     "0.1.1",
		Description: "Default Xira runtime assistant for channel entrypoints and operational guidance.",
		ModelPolicy: ModelPolicy{
			Provider: "deepseek",
			Model:    "deepseek-v4-flash",
			Stream:   true,
			Temp:     0.2,
		},
		Instructions: []string{
			"You are Xira's default runtime assistant.",
			"Reply directly to the user in the user's language.",
			"Do not pretend a specialized agent or flow is active unless the user explicitly invokes one.",
			"When useful, mention the exact Xira command the user can run, such as /agents, /agent <id> <message>, /use <id>, or /flows.",
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
		Version:     "0.1.1",
		Description: "Local-first research assistant for evidence search, summaries, and Xira Phase 1 validation.",
		ModelPolicy: ModelPolicy{
			Provider: "deepseek",
			Model:    "deepseek-v4-flash",
			Stream:   true,
			Temp:     0.2,
		},
		Instructions: []string{
			"You are Xira's built-in research assistant.",
			"Prefer local evidence and tool results over unsupported guesses.",
			"When using external commands, stay within runtime policy and summarize outputs with source paths.",
		},
		Permissions:  Permissions{Tools: BuiltinToolNames()},
		Verification: VerificationPolicy{DefaultChecks: []string{"final_response_non_empty"}},
		Artifacts:    ArtifactPolicy{OutputDir: "artifacts", Retention: "local"},
		Evolution:    EvolutionPolicy{Enabled: true, CandidateOnly: true},
	}
}

func BuiltinToolNames() []string {
	return []string{"exec", "read_file", "search_file", "write_file", "list_dir", "edit_file"}
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
