package feishu

import (
	"context"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/xiramesh/xira/internal/channel"
)

// extractMentionTargets preserves each structured Feishu mention as a canonical identity.
// coverage: contract (100% required)
func extractMentionTargets(mentions []*larkim.MentionEvent) []channel.MentionTarget {
	targets := make([]channel.MentionTarget, 0, len(mentions))
	for _, mention := range mentions {
		if mention == nil {
			continue
		}
		id, idType := canonicalUserIdentity(mention.Id)
		if id == "" {
			continue
		}
		targets = append(targets, channel.MentionTarget{
			Key:    stringValue(mention.Key),
			ID:     id,
			IDType: idType,
			Name:   stringValue(mention.Name),
		})
	}
	return targets
}

// canonicalUserIdentity keeps sender and mention identity selection symmetric.
// coverage: contract (100% required)
func canonicalUserIdentity(id *larkim.UserId) (string, string) {
	if id == nil {
		return "", ""
	}
	if value := stringValue(id.UserId); value != "" {
		return value, "user_id"
	}
	if value := stringValue(id.OpenId); value != "" {
		return value, "open_id"
	}
	if value := stringValue(id.UnionId); value != "" {
		return value, "union_id"
	}
	return "", ""
}

// isOwnerMentioned performs strict entrypoint-scoped owner matching.
// coverage: contract (100% required)
func (r *Runner) isOwnerMentioned(ctx context.Context, targets []channel.MentionTarget) bool {
	if r == nil || r.ownerResolver == nil {
		return false
	}
	for _, target := range targets {
		if target.ID != "" && r.ownerResolver.IsOwner(ctx, target.ID, r.definition.ID) {
			return true
		}
	}
	return false
}
