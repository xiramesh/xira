package entrypoints

import (
	"fmt"
	"path"
	"strings"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/routing"
)

type Definition struct {
	ID                                string                `json:"id" yaml:"id"`
	Enabled                           bool                  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Channel                           string                `json:"channel" yaml:"channel"`
	Account                           string                `json:"account,omitempty" yaml:"account,omitempty"`
	AppID                             string                `json:"app_id,omitempty" yaml:"app_id,omitempty"`
	AppIDEnv                          string                `json:"app_id_env,omitempty" yaml:"app_id_env,omitempty"`
	BotID                             string                `json:"bot_id,omitempty" yaml:"bot_id,omitempty"`
	Token                             string                `json:"token,omitempty" yaml:"token,omitempty"`
	TokenEnv                          string                `json:"token_env,omitempty" yaml:"token_env,omitempty"`
	BaseURL                           string                `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	BaseURLEnv                        string                `json:"base_url_env,omitempty" yaml:"base_url_env,omitempty"`
	StateDir                          string                `json:"state_dir,omitempty" yaml:"state_dir,omitempty"`
	DefaultAgentID                    string                `json:"default_agent" yaml:"default_agent"`
	AllowedAgentIDs                   []string              `json:"allowed_agents,omitempty" yaml:"allowed_agents,omitempty"`
	AllowedSenderIDs                  []string              `json:"allowed_senders,omitempty" yaml:"allowed_senders,omitempty"`
	OwnerID                           string                `json:"owner,omitempty" yaml:"owner,omitempty"`
	SessionPolicy                     routing.SessionPolicy `json:"session,omitempty" yaml:"session,omitempty"`
	AppSecret                         string                `json:"app_secret,omitempty" yaml:"app_secret,omitempty"`
	AppSecretEnv                      string                `json:"app_secret_env,omitempty" yaml:"app_secret_env,omitempty"`
	EncryptKey                        string                `json:"encrypt_key,omitempty" yaml:"encrypt_key,omitempty"`
	EncryptKeyEnv                     string                `json:"encrypt_key_env,omitempty" yaml:"encrypt_key_env,omitempty"`
	VerifyToken                       string                `json:"verification_token,omitempty" yaml:"verification_token,omitempty"`
	VerifyTokenEnv                    string                `json:"verification_token_env,omitempty" yaml:"verification_token_env,omitempty"`
	IsLark                            bool                  `json:"is_lark,omitempty" yaml:"is_lark,omitempty"`
	AllowRuntimePairing               bool                  `json:"allow_runtime_pairing,omitempty" yaml:"allow_runtime_pairing,omitempty"`
	RespondToUnmentionedGroupMessages bool                  `json:"respond_to_unmentioned_group_messages,omitempty" yaml:"respond_to_unmentioned_group_messages,omitempty"`
	DataIsolation                     DataIsolationPolicy   `json:"data_isolation,omitempty" yaml:"data_isolation,omitempty"`
}

// DataIsolationPolicy 控制 per-sender 工具数据隔离（#126）。Enabled=true 时，
// 该 entrypoint 的工具写入落 workspace/users/{sender}/，读走 overlay（私有优先 +
// fallback 通用层）。不配或 Enabled=false → 维持单层（向后兼容）。
type DataIsolationPolicy struct {
	Enabled bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

type ResolveInput struct {
	Context          channel.InboundContext
	EntrypointID     string
	RequestedAgentID string
}

type Decision struct {
	Definition    Definition            `json:"definition" yaml:"definition"`
	AgentID       string                `json:"agent_id" yaml:"agent_id"`
	SessionPolicy routing.SessionPolicy `json:"session_policy" yaml:"session_policy"`
	MatchedBy     string                `json:"matched_by" yaml:"matched_by"`
}

type Registry struct {
	defaultAgentID string
	definitions    []Definition
	byID           map[string]Definition
}

func NewRegistry(defaultAgentID string, definitions []Definition) *Registry {
	defaultAgentID = strings.TrimSpace(defaultAgentID)
	if defaultAgentID == "" {
		defaultAgentID = "xira-assistant"
	}
	registry := &Registry{
		defaultAgentID: defaultAgentID,
		byID:           map[string]Definition{},
	}
	for _, definition := range definitions {
		normalized := normalizeDefinition(definition, defaultAgentID)
		if normalized.ID == "" || normalized.Channel == "" {
			continue
		}
		registry.definitions = append(registry.definitions, normalized)
		registry.byID[normalized.ID] = normalized
	}
	return registry
}

func (r *Registry) Resolve(input ResolveInput) (Decision, error) {
	if r == nil {
		r = NewRegistry("", nil)
	}
	ctx := channel.NormalizeInboundContext(input.Context)
	entrypointID := strings.TrimSpace(input.EntrypointID)
	if entrypointID == "" {
		entrypointID = ctx.EntrypointID
	}
	var definition Definition
	matchedBy := "entrypoint.implicit"
	if entrypointID != "" {
		found, ok := r.byID[entrypointID]
		if !ok {
			return Decision{}, fmt.Errorf("entrypoint %q not found", entrypointID)
		}
		definition = found
		matchedBy = "entrypoint.request"
	} else if found, ok := r.matchContext(ctx); ok {
		definition = found
		matchedBy = "entrypoint.channel"
	} else {
		definition = implicitDefinition(ctx, r.defaultAgentID)
	}
	agentID := strings.TrimSpace(input.RequestedAgentID)
	if agentID != "" {
		if !definition.AllowsAgent(agentID) {
			return Decision{}, fmt.Errorf("agent %q is not allowed by entrypoint %q", agentID, definition.ID)
		}
		matchedBy = "request.agent_id"
	} else {
		agentID = definition.DefaultAgentID
	}
	return Decision{
		Definition:    definition,
		AgentID:       agentID,
		SessionPolicy: routing.NormalizeSessionPolicy(definition.SessionPolicy),
		MatchedBy:     matchedBy,
	}, nil
}

func (r *Registry) Definitions() []Definition {
	if r == nil {
		return nil
	}
	out := make([]Definition, len(r.definitions))
	copy(out, r.definitions)
	return out
}

// Definition returns the entrypoint definition by ID. ok=false if not found.
// Used by Service.IsOwner (#122) to look up an entrypoint's declared owner
// without going through full Resolve (which also resolves agent IDs).
func (r *Registry) Definition(id string) (Definition, bool) {
	if r == nil {
		return Definition{}, false
	}
	def, ok := r.byID[strings.TrimSpace(id)]
	return def, ok
}

func (r *Registry) matchContext(ctx channel.InboundContext) (Definition, bool) {
	var best Definition
	bestScore := -1
	for _, definition := range r.definitions {
		if !definition.matches(ctx) {
			continue
		}
		score := definition.matchScore()
		if score > bestScore {
			best = definition
			bestScore = score
		}
	}
	if bestScore < 0 {
		return Definition{}, false
	}
	return best, true
}

func (d Definition) AllowsAgent(agentID string) bool {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false
	}
	if len(d.AllowedAgentIDs) == 0 {
		return true
	}
	for _, allowed := range d.AllowedAgentIDs {
		if allowed == agentID {
			return true
		}
	}
	return false
}

// AllowsSender reports whether senderID is authorized to use this entrypoint.
// Matching uses path.Match (glob): "ou_*", or exact IDs.
//
// Special case: bare "*" is handled BEFORE path.Match. Go's path.Match
// defines "*" to match only non-"/" characters, so path.Match("*", "a/b")
// returns false — but sender IDs CAN contain "/" (chatkey_test pins
// "sender with slash preserved"). Without this special case, an explicit
// allowed_senders: ["*"] would silently reject senders that an empty
// allowlist (functionally equivalent per the contract) would accept.
// The special case makes "*" truly equivalent to "allow any non-empty sender".
//
//   - Empty senderID → rejected.
//   - Empty AllowedSenderIDs → allow all (backward compat, matches AllowedAgentIDs).
//   - pattern == "*" → allow any non-empty sender (bypasses path.Match).
//   - Other patterns → path.Match; malformed (ErrBadPattern) skipped, not fail-open.
func (d Definition) AllowsSender(senderID string) bool {
	senderID = strings.TrimSpace(senderID)
	if senderID == "" {
		return false
	}
	if len(d.AllowedSenderIDs) == 0 {
		return true
	}
	for _, pattern := range d.AllowedSenderIDs {
		if pattern == "*" {
			return true // bypass path.Match — see comment above
		}
		ok, err := path.Match(pattern, senderID)
		if err != nil {
			continue // ErrBadPattern: skip, do not fail-open
		}
		if ok {
			return true
		}
	}
	return false
}

func (d Definition) matches(ctx channel.InboundContext) bool {
	if d.Channel != ctx.Channel {
		return false
	}
	if d.Account != "" && !strings.EqualFold(d.Account, ctx.Account) {
		return false
	}
	if d.AppID != "" && !strings.EqualFold(d.AppID, ctx.ChannelAppID) {
		return false
	}
	if d.BotID != "" && !strings.EqualFold(d.BotID, ctx.BotID) {
		return false
	}
	return true
}

func (d Definition) matchScore() int {
	score := 0
	if d.Account != "" {
		score++
	}
	if d.AppID != "" {
		score++
	}
	if d.BotID != "" {
		score++
	}
	return score
}

func normalizeDefinition(definition Definition, defaultAgentID string) Definition {
	definition.ID = strings.TrimSpace(definition.ID)
	definition.Channel = strings.ToLower(strings.TrimSpace(definition.Channel))
	definition.Account = strings.TrimSpace(definition.Account)
	definition.AppID = strings.TrimSpace(definition.AppID)
	definition.AppIDEnv = strings.TrimSpace(definition.AppIDEnv)
	definition.BotID = strings.TrimSpace(definition.BotID)
	definition.Token = strings.TrimSpace(definition.Token)
	definition.TokenEnv = strings.TrimSpace(definition.TokenEnv)
	definition.BaseURL = strings.TrimSpace(definition.BaseURL)
	definition.BaseURLEnv = strings.TrimSpace(definition.BaseURLEnv)
	definition.StateDir = strings.TrimSpace(definition.StateDir)
	definition.DefaultAgentID = strings.TrimSpace(definition.DefaultAgentID)
	definition.AppSecret = strings.TrimSpace(definition.AppSecret)
	definition.AppSecretEnv = strings.TrimSpace(definition.AppSecretEnv)
	definition.EncryptKey = strings.TrimSpace(definition.EncryptKey)
	definition.EncryptKeyEnv = strings.TrimSpace(definition.EncryptKeyEnv)
	definition.VerifyToken = strings.TrimSpace(definition.VerifyToken)
	definition.VerifyTokenEnv = strings.TrimSpace(definition.VerifyTokenEnv)
	if definition.DefaultAgentID == "" {
		definition.DefaultAgentID = defaultAgentID
	}
	allowed := make([]string, 0, len(definition.AllowedAgentIDs))
	seen := map[string]struct{}{}
	for _, agentID := range definition.AllowedAgentIDs {
		agentID = strings.TrimSpace(agentID)
		if agentID == "" {
			continue
		}
		if _, ok := seen[agentID]; ok {
			continue
		}
		seen[agentID] = struct{}{}
		allowed = append(allowed, agentID)
	}
	definition.AllowedAgentIDs = allowed
	allowedSenders := make([]string, 0, len(definition.AllowedSenderIDs))
	seenSenders := map[string]struct{}{}
	for _, senderID := range definition.AllowedSenderIDs {
		senderID = strings.TrimSpace(senderID)
		if senderID == "" {
			continue
		}
		if _, ok := seenSenders[senderID]; ok {
			continue
		}
		seenSenders[senderID] = struct{}{}
		allowedSenders = append(allowedSenders, senderID)
	}
	definition.AllowedSenderIDs = allowedSenders
	definition.OwnerID = strings.TrimSpace(definition.OwnerID)
	definition.SessionPolicy = routing.NormalizeSessionPolicy(definition.SessionPolicy)
	return definition
}

func implicitDefinition(ctx channel.InboundContext, defaultAgentID string) Definition {
	channelName := strings.ToLower(strings.TrimSpace(ctx.Channel))
	if channelName == "" {
		channelName = "local"
	}
	return Definition{
		ID:             channelName + "-default",
		Channel:        channelName,
		Account:        strings.TrimSpace(ctx.Account),
		DefaultAgentID: defaultAgentID,
		SessionPolicy: routing.SessionPolicy{
			Dimensions: routing.DefaultSessionDimensions,
		},
	}
}
