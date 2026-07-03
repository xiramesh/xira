package runtime

import (
	"strconv"
	"strings"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// human_request_hydration.go: #106 — inject pending HITL summary into a fresh
// RunAgent turn's user message, so the agent knows what human input is
// currently awaiting an answer for this chatKey.
//
// Design (see #106 / #105):
//   - Only pending (unresolved) HumanRequests for the current chatKey are
//     injected. They are sourced from ListPendingHumanRequestsByChatKey.
//   - The summary is appended to the user message (NOT the system/instruction
//     text), so it only affects the current RunAgent turn — resume and child
//     delegation turns are untouched (their chatKey/semantics differ, and
//     resume already carries resolved-HITL context in its own message).
//   - Sensitive ActionSnapshot fields (tool arguments, file contents from a
//     runtime_tool_gate) are NEVER included — only request_id, source, kind,
//     question, and options. The agent gets enough to recognize/quote the
//     pending request, not the data inside the gated action.

// pendingHITLSummaryMarker is the stable heading the tests and downstream pin
// on. It is rendered exactly once per turn when ≥1 pending HITL is injected.
const pendingHITLSummaryMarker = "# Pending Human Requests"

// injectPendingHITLSummary appends a structured "Pending Human Requests"
// block to the user message when there are pending HITL requests for this
// chatKey. If pending is empty, msg is returned unchanged.
//
// The block intentionally excludes ActionSnapshot arguments: a runtime_tool_gate
// on write_file must not leak the file contents into the agent context. Only
// the tool name is surfaced (so the agent can reference what kind of action is
// gated), plus request_id / source / kind / question / options.
func injectPendingHITLSummary(msg string, pending []humanrequest.HumanRequest) string {
	msg = strings.TrimSpace(msg)
	if len(pending) == 0 {
		return msg
	}
	var b strings.Builder
	b.WriteString(msg)
	b.WriteString("\n\n")
	b.WriteString(pendingHITLSummaryMarker)
	b.WriteString(" (awaiting human input)\n")
	for i, hr := range pending {
		b.WriteString(formatPendingHumanRequest(strconv.Itoa(i+1), hr))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString("If this reply is answering one of the requests above, identify which one from context; if it is ambiguous, ask the user to clarify rather than guessing.")
	return b.String()
}

// formatPendingHumanRequest renders a single pending request as one numbered
// line. It deliberately omits ActionSnapshot.Arguments — only the tool name
// (from ActionSnapshot.ToolName) is kept so the agent can reference the gated
// action type without seeing its (possibly sensitive) inputs.
func formatPendingHumanRequest(n string, hr humanrequest.HumanRequest) string {
	var b strings.Builder
	b.WriteString(n)
	b.WriteString(". [")
	source := strings.TrimSpace(hr.Source)
	if source == "" {
		source = "human_request"
	}
	b.WriteString(source)
	b.WriteString("] ")
	b.WriteString(strings.TrimSpace(hr.Question))
	// For runtime_tool_gate, surface the tool name (not its arguments) so the
	// agent can reference the gated action. This is the only ActionSnapshot
	// field exposed.
	if hr.ActionSnapshot != nil && strings.TrimSpace(hr.ActionSnapshot.ToolName) != "" {
		b.WriteString(" (tool: ")
		b.WriteString(strings.TrimSpace(hr.ActionSnapshot.ToolName))
		b.WriteString(")")
	}
	// Options, if any — so the agent can guide the user and #108's option
	// matching has the same surface visible.
	if len(hr.Options) > 0 {
		b.WriteString(" (options: ")
		opts := make([]string, 0, len(hr.Options))
		for _, o := range hr.Options {
			opts = append(opts, formatHumanOption(o))
		}
		b.WriteString(strings.Join(opts, ", "))
		b.WriteString(")")
	}
	if id := strings.TrimSpace(hr.ID); id != "" {
		b.WriteString("  [request_id: ")
		b.WriteString(id)
		b.WriteString("]")
	}
	return b.String()
}

func formatHumanOption(o humanrequest.HumanOption) string {
	id := strings.TrimSpace(o.ID)
	label := strings.TrimSpace(o.Label)
	if id != "" && label != "" && id != label {
		return id + "/" + label
	}
	if id != "" {
		return id
	}
	return label
}
