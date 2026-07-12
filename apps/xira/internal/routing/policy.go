package routing

import "strings"

// DefaultSessionDimensions 是 session 隔离的唯一维度配置。
// #151：session 只按 chat 分（群聊=整个群一个 session，私聊=一个对话一个 session）。
// per-sender 的东西（user.md / memory）已独立到 stateDir，不在 session 里。
// dimensions 配置项不再暴露给部署者——硬编码 [chat]，没有别的合理选择。
var DefaultSessionDimensions = []string{"chat"}

type SessionPolicy struct {
	// Dimensions 不再从 yaml 配置读取——NormalizeSessionPolicy 始终用 DefaultSessionDimensions。
	// 字段保留是为了向后兼容（旧 yaml 里有 dimensions 不报错，但被忽略）。
	Dimensions    []string            `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
	IdentityLinks map[string][]string `json:"identity_links,omitempty" yaml:"identity_links,omitempty"`
}

func NormalizeSessionPolicy(policy SessionPolicy) SessionPolicy {
	// #151：始终用 [chat]，忽略配置里的 dimensions。
	policy.Dimensions = normalizeSessionDimensions(DefaultSessionDimensions)
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
