package agents

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
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
	Delegation   DelegationPolicy   `json:"delegation,omitempty" yaml:"delegation,omitempty"`
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

type DelegationPolicy struct {
	Enabled                 bool     `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Allow                   []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	MaxDepth                int      `json:"max_depth,omitempty" yaml:"max_depth,omitempty"`
	MaxParallel             int      `json:"max_parallel,omitempty" yaml:"max_parallel,omitempty"`
	DefaultMaxDurationMS    int      `json:"default_max_duration_ms,omitempty" yaml:"default_max_duration_ms,omitempty"`
	MaxDurationMS           int      `json:"max_duration_ms,omitempty" yaml:"max_duration_ms,omitempty"`
	ExposeChildOutputToUser bool     `json:"expose_child_output_to_user,omitempty" yaml:"expose_child_output_to_user,omitempty"`
	ReturnTo                string   `json:"return_to,omitempty" yaml:"return_to,omitempty"`
	ChildSessionMode        string   `json:"child_session_mode,omitempty" yaml:"child_session_mode,omitempty"`
	maxDepthConfigured      bool
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
		Permissions: Permissions{Tools: BuiltinToolNames()},
		Delegation: DelegationPolicy{
			Enabled: true,
			Allow:   []string{ResearchAssistantAgentID},
		},
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

func (p Profile) NormalizedDelegationPolicy() DelegationPolicy {
	return NormalizeDelegationPolicy(p.Delegation)
}

func (p *DelegationPolicy) UnmarshalYAML(node *yaml.Node) error {
	type delegationPolicy DelegationPolicy
	var decoded delegationPolicy
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*p = DelegationPolicy(decoded)
	p.maxDepthConfigured = yamlMappingHasKey(node, "max_depth")
	return nil
}

func NormalizeDelegationPolicy(policy DelegationPolicy) DelegationPolicy {
	normalized := policy
	normalized.Allow = compactStrings(normalized.Allow)
	if normalized.MaxDepth == 0 && !normalized.maxDepthConfigured {
		normalized.MaxDepth = 1
	}
	if normalized.MaxParallel == 0 {
		normalized.MaxParallel = 1
	}
	if normalized.DefaultMaxDurationMS == 0 {
		normalized.DefaultMaxDurationMS = 30000
	}
	if normalized.MaxDurationMS == 0 {
		normalized.MaxDurationMS = 120000
	}
	if strings.TrimSpace(normalized.ReturnTo) == "" {
		normalized.ReturnTo = "caller"
	}
	if strings.TrimSpace(normalized.ChildSessionMode) == "" {
		normalized.ChildSessionMode = "ephemeral_worker"
	}
	return normalized
}

func (p DelegationPolicy) Allows(agentID string) bool {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false
	}
	for _, allowed := range p.Allow {
		if strings.TrimSpace(allowed) == agentID {
			return true
		}
	}
	return false
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
	if err := validateDelegationPolicy(p.NormalizedDelegationPolicy()); err != nil {
		return err
	}
	return nil
}

func validateDelegationPolicy(policy DelegationPolicy) error {
	var errs []string
	if policy.MaxDepth < 0 {
		errs = append(errs, "delegation.max_depth must be >= 0")
	}
	if policy.MaxDepth > 1 {
		errs = append(errs, "delegation.max_depth must be 0 or 1 in Phase 1")
	}
	if policy.MaxParallel < 1 {
		errs = append(errs, "delegation.max_parallel must be >= 1")
	}
	if policy.DefaultMaxDurationMS < 1 {
		errs = append(errs, "delegation.default_max_duration_ms must be >= 1")
	}
	if policy.MaxDurationMS < policy.DefaultMaxDurationMS {
		errs = append(errs, "delegation.max_duration_ms must be >= delegation.default_max_duration_ms")
	}
	switch policy.ReturnTo {
	case "caller":
	default:
		errs = append(errs, "delegation.return_to must be caller")
	}
	switch policy.ChildSessionMode {
	case "ephemeral_worker":
	default:
		errs = append(errs, "delegation.child_session_mode must be ephemeral_worker in Phase 1")
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func yamlMappingHasKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}
	return false
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
