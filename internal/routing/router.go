package routing

import (
	"strings"

	"github.com/ai-daming/xira/internal/channel"
)

var DefaultSessionDimensions = []string{"chat", "sender"}

type SessionPolicy struct {
	Dimensions    []string            `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	IdentityLinks map[string][]string `json:"identity_links,omitempty" yaml:"identity_links,omitempty"`
}

type Decision struct {
	AgentID       string        `json:"agent_id" yaml:"agent_id"`
	Channel       string        `json:"channel" yaml:"channel"`
	AccountID     string        `json:"account_id,omitempty" yaml:"account_id,omitempty"`
	SessionPolicy SessionPolicy `json:"session_policy" yaml:"session_policy"`
	MatchedBy     string        `json:"matched_by" yaml:"matched_by"`
}

type Rule struct {
	Channel       string        `json:"channel,omitempty" yaml:"channel,omitempty"`
	AgentID       string        `json:"agent_id" yaml:"agent_id"`
	SessionPolicy SessionPolicy `json:"session_policy,omitempty" yaml:"session_policy,omitempty"`
}

type Router struct {
	defaultAgentID string
	rules          []Rule
}

func NewRouter(defaultAgentID string) *Router {
	return NewRouterWithRules(defaultAgentID, nil)
}

func NewRouterWithRules(defaultAgentID string, rules []Rule) *Router {
	defaultAgentID = strings.TrimSpace(defaultAgentID)
	out := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		rule.Channel = strings.ToLower(strings.TrimSpace(rule.Channel))
		rule.AgentID = strings.TrimSpace(rule.AgentID)
		rule.SessionPolicy = NormalizeSessionPolicy(rule.SessionPolicy)
		if rule.Channel == "" || rule.AgentID == "" {
			continue
		}
		out = append(out, rule)
	}
	return &Router{defaultAgentID: defaultAgentID, rules: out}
}

func (r *Router) Resolve(ctx channel.InboundContext, requestedAgentID string) Decision {
	ctx = channel.NormalizeInboundContext(ctx)
	agentID := strings.TrimSpace(requestedAgentID)
	matchedBy := "request.agent_id"
	sessionPolicy := NormalizeSessionPolicy(SessionPolicy{
		Dimensions: DefaultSessionDimensions,
	})
	if agentID == "" {
		agentID = r.defaultAgentID
		matchedBy = "default"
		for _, rule := range r.rules {
			if rule.Channel == ctx.Channel {
				agentID = rule.AgentID
				sessionPolicy = rule.SessionPolicy
				matchedBy = "route.channel"
				break
			}
		}
	}
	return Decision{
		AgentID:       agentID,
		Channel:       ctx.Channel,
		AccountID:     strings.TrimSpace(ctx.Account),
		SessionPolicy: sessionPolicy,
		MatchedBy:     matchedBy,
	}
}

func NormalizeSessionPolicy(policy SessionPolicy) SessionPolicy {
	policy.Dimensions = normalizeSessionDimensions(policy.Dimensions)
	if len(policy.Dimensions) == 0 {
		policy.Dimensions = normalizeSessionDimensions(DefaultSessionDimensions)
	}
	if len(policy.IdentityLinks) == 0 {
		policy.IdentityLinks = nil
	}
	return policy
}

func normalizeSessionDimensions(dimensions []string) []string {
	if len(dimensions) == 0 {
		return nil
	}
	out := make([]string, 0, len(dimensions))
	seen := map[string]struct{}{}
	for _, dimension := range dimensions {
		dimension = strings.ToLower(strings.TrimSpace(dimension))
		switch dimension {
		case "space", "chat", "topic", "sender", "channel":
		default:
			continue
		}
		if _, ok := seen[dimension]; ok {
			continue
		}
		seen[dimension] = struct{}{}
		out = append(out, dimension)
	}
	return out
}
