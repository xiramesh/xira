package progress

import (
	"strings"
	"unicode/utf8"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// hitl_classify.go: maps a user's IM text reply to a HITL ResponseKind.
// Used by channel adapters (#92 — HITL IM direct answer) to resolve a pending
// HumanRequest from a plain-text message, without the user leaving the IM.
//
// Mapping rules:
//   - RequestFreeform: the user's text IS the answer → ResponseAnswer + text as Message.
//   - RequestApproval: keyword match (case-insensitive, trimmed):
//       approve: 同意/是/好/ok/yes/approve/确认/对/可以
//       deny:    拒绝/否/no/deny/不行/不对
//       cancel:  取消/cancel/算了/不要了
//     No keyword match → short replies (≤2 runes) default to approve; longer
//     text treated as a free-form answer to the question.
//
// Channel adapters can replace this with their own logic (e.g. button card →
// exact kind, locale-specific keywords). This is the shared default.

// ClassifyHITLResponse maps user text to (ResponseKind, message).
// reqKind is the pending HumanRequest's Kind (RequestApproval or RequestFreeform).
func ClassifyHITLResponse(text string, reqKind humanrequest.RequestKind) (humanrequest.ResponseKind, string) {
	text = strings.TrimSpace(text)
	if reqKind == humanrequest.RequestFreeform {
		// Freeform: text is the answer. Message must be non-empty for answer kind.
		if text == "" {
			text = " "
		}
		return humanrequest.ResponseAnswer, text
	}
	// Approval: keyword match.
	lower := strings.ToLower(text)
	switch {
	case matchAny(lower, "同意", "是", "好", "ok", "yes", "approve", "确认", "对", "可以", "y"):
		return humanrequest.ResponseApprove, text
	case matchAny(lower, "拒绝", "否", "不好", "no", "deny", "不行", "不对", "n"):
		return humanrequest.ResponseDeny, text
	case matchAny(lower, "取消", "cancel", "算了", "不要了"):
		return humanrequest.ResponseCancel, text
	}
	// No keyword match on an approval request. Short replies default to approve
	// (most common intent); longer text is treated as a free-form answer.
	if utf8.RuneCountInString(text) <= 2 {
		return humanrequest.ResponseApprove, text
	}
	return humanrequest.ResponseAnswer, text
}

func matchAny(s string, words ...string) bool {
	for _, w := range words {
		if s == w {
			return true
		}
	}
	return false
}
