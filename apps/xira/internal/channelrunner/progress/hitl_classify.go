package progress

import (
	"strings"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// hitl_classify.go: maps a user's IM text reply to a HITL ResponseKind (#92).
//
// Design: for pure-text channels (no button card / interactive UI), the user's
// text is ALWAYS treated as a free-form answer (ResponseAnswer + text as Message).
// No keyword matching — that's a rules engine that can never cover all ways a
// user expresses intent (e.g. "不要" is neither approve nor deny in a keyword
// list, but the user clearly means no). Intent understanding is left to the
// agent (LLM) during resume.
//
// Channels with interactive UI (feishu button card — future) can construct a
// precise ResponseKind (approve/deny/cancel) directly from the button click,
// bypassing this function entirely.

// ClassifyHITLResponse maps user text to (ResponseKind, message) for pure-text
// channels. It always returns ResponseAnswer + the user's text — the agent
// decides what the user means during resume. reqKind is accepted for API
// stability (future channels with buttons may use it) but ignored today.
func ClassifyHITLResponse(text string, _ humanrequest.RequestKind) (humanrequest.ResponseKind, string) {
	text = strings.TrimSpace(text)
	// Message must be non-empty for answer kind (Store validates this).
	if text == "" {
		text = " "
	}
	return humanrequest.ResponseAnswer, text
}
