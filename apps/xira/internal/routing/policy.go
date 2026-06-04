package routing

import "strings"

var DefaultSessionDimensions = []string{"chat", "sender"}

type SessionPolicy struct {
	Dimensions    []string            `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	IdentityLinks map[string][]string `json:"identity_links,omitempty" yaml:"identity_links,omitempty"`
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
