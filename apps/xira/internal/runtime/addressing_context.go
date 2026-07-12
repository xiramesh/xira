package runtime

import (
	"strings"

	"github.com/xiramesh/xira/internal/channel"
)

// formatAddressingContext turns sealed address facts into the model's role contract.
// coverage: contract (100% required)
func formatAddressingContext(inbound channel.InboundContext) string {
	hasAgent := false
	hasOwner := false
	for _, target := range inbound.AddressedTo {
		switch target {
		case channel.AddressTargetAgent:
			hasAgent = true
		case channel.AddressTargetOwner:
			hasOwner = true
		}
	}
	if !hasAgent && !hasOwner {
		return ""
	}
	targets := make([]string, 0, 2)
	if hasAgent {
		targets = append(targets, string(channel.AddressTargetAgent))
	}
	if hasOwner {
		targets = append(targets, string(channel.AddressTargetOwner))
	}
	lines := []string{"Addressed to: " + strings.Join(targets, ", ")}
	if hasOwner {
		lines = append(lines,
			"This turn explicitly addresses the owner bound to this agent. You are the owner's AI intern, not the owner. Use the complete conversation context and available tools to judge whether to stay silent, respond as yourself, prepare useful work, or request the owner's decision. Never impersonate the owner or make decisions, commitments, approvals, or denials on the owner's behalf.",
		)
	}
	return strings.Join(lines, "\n")
}
