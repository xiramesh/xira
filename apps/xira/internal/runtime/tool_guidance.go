package runtime

import (
	"context"
	"sort"
	"strings"

	"github.com/xiramesh/xira/internal/agents"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

const (
	humanRequestToolGuidance   = "Use this only when the current run cannot responsibly continue without a specific human answer, choice, or approval. Ask one precise question with enough context for the human to decide. Do not pause for low-stakes ambiguity that you can resolve from the conversation, available evidence, or reasonable judgment. A successful call suspends the run, so do not continue as if the answer already exists."
	humanInterpretToolGuidance = "Use this only when the current message clearly answers one specific pending human request shown in the injected summary. Bind the response to the exact request id and represent the sender's actual intent without embellishment. If several pending requests could match, or the reply is genuinely ambiguous, do not guess or resolve any of them."
	notifyOwnerToolGuidance    = "Use this when the configured owner should privately know something learned or prepared in the current turn. Include who is involved, the relevant context, and what the owner needs to know, while never implying that the owner approved, decided, promised, or acted. Do not send a notification for information that only needs to remain internal or for routine conversation that needs no owner attention."
	finishSilentToolGuidance   = "Use this only after all required work in the current turn succeeded and you independently conclude that no public response or private notification is needed. It is appropriate when the useful outcome is an internal state change or a deliberate no-op. Never use silence instead of answering a real question, reporting a material blocker, or acknowledging a failed or rejected action."
	statusToolGuidance         = "Use this for meaningful progress during work that takes long enough for the user to benefit from an update. Report concrete state changes, important waits, or blockers; do not emit routine narration for every step. A progress event is not the final answer, so completing the task still requires an actual final outcome."
	spawnTurnToolGuidance      = "Use this for a bounded subtask that another available Agent can perform independently or in parallel without taking over your responsibility for the user's request. Pass a self-contained objective and all context the child needs. Do not delegate trivial work, a single mechanical action, or the entire task merely to avoid doing it yourself; verify and synthesize the child's result before relying on it."
	pollTurnToolGuidance       = "Use this only to inspect a known child turn id whose result matters to the current work. A pending result means the child is still running: continue other useful work or check later instead of repeatedly polling in a tight loop. Treat a completed child summary as evidence to evaluate, not as an automatically verified final answer."
	answerChildToolGuidance    = "Use this only when a child is waiting on a specific question and the available context gives you a well-supported answer. Answer the question asked without inventing user or owner intent. If the answer requires a human decision, unavailable fact, or authority you do not possess, leave it for the human rather than guessing on their behalf."
)

var baseRuntimeToolNames = []string{
	"human.request",
	notifyOwnerToolName,
	finishSilentToolName,
	humanInterpretToolName,
	statusToolName,
}

var delegationRuntimeToolNames = []string{
	spawnTurnToolName,
	pollTurnToolName,
	answerChildToolName,
}

// effectiveToolNames returns the exact model-visible ADK tool names after all
// current-turn gates. It is a contract boundary: Guidance must never be
// compiled from the broader configured tool set.
// coverage: contract (100% required)
func (s *Service) effectiveToolNames(ctx context.Context, profile agents.Profile) []string {
	seen := map[string]struct{}{}
	if !runtimeNativeToolsDisabledFromContext(ctx) {
		for _, name := range baseRuntimeToolNames {
			if runtimeToolAllowedFromContext(ctx, name) {
				seen[name] = struct{}{}
			}
		}
		if profile.NormalizedDelegationPolicy().Enabled {
			for _, name := range delegationRuntimeToolNames {
				if runtimeToolAllowedFromContext(ctx, name) {
					seen[name] = struct{}{}
				}
			}
		}
	}
	for _, def := range s.toolRegistry(profile).Definitions() {
		if runtimeToolAllowedFromContext(ctx, def.Name) {
			seen[def.Name] = struct{}{}
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Service) canonicalGuidanceToolName(profile agents.Profile, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, candidate := range append(append([]string{}, baseRuntimeToolNames...), delegationRuntimeToolNames...) {
		if name == candidate || name == deepseek.DeepSeekToolName(candidate) {
			return candidate
		}
	}
	for _, def := range s.toolRegistry(profile).Definitions() {
		if name == def.Name || name == deepseek.DeepSeekToolName(def.Name) {
			return def.Name
		}
	}
	return name
}

func (s *Service) toolGuidance(profile agents.Profile, name string) string {
	name = s.canonicalGuidanceToolName(profile, name)
	switch name {
	case "human.request":
		return humanRequestToolGuidance
	case notifyOwnerToolName:
		return notifyOwnerToolGuidance
	case finishSilentToolName:
		return finishSilentToolGuidance
	case humanInterpretToolName:
		return humanInterpretToolGuidance
	case statusToolName:
		return statusToolGuidance
	case spawnTurnToolName:
		return spawnTurnToolGuidance
	case pollTurnToolName:
		return pollTurnToolGuidance
	case answerChildToolName:
		return answerChildToolGuidance
	}
	for _, def := range s.toolRegistry(profile).Definitions() {
		if def.Name == name {
			return strings.TrimSpace(def.Guidance)
		}
	}
	return ""
}

func (s *Service) compileToolGuidance(profile agents.Profile, names []string) string {
	seen := map[string]struct{}{}
	for _, name := range names {
		canonical := s.canonicalGuidanceToolName(profile, name)
		if canonical != "" && strings.TrimSpace(s.toolGuidance(profile, canonical)) != "" {
			seen[canonical] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(seen))
	for name := range seen {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	if len(ordered) == 0 {
		return ""
	}
	parts := []string{"# Tool Guidance"}
	for _, name := range ordered {
		parts = append(parts, "## "+name+"\n\n"+strings.TrimSpace(s.toolGuidance(profile, name)))
	}
	return strings.Join(parts, "\n\n")
}
