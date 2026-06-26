package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// poll_turn.go: the parent LLM's NON-BLOCKING tool to check a spawned child's
// result (Phase 4, RFC §2.4 D-3). After spawn_turn returns {agent_turn_id,
// status:spawned}, the parent may call poll_turn(child_turn_id) to check
// whether that child has finished — returning immediately with the result if
// done, or {status:"pending"} if still running.
//
// CRITICAL design constraint (PR #53 review): this tool MUST NOT block. ADK
// v1.4.0 runs tools synchronously (base_flow.go wg.Wait), so a blocking tool
// freezes the event-yielding iterator and disables the steering checkpoint
// (service_adk.go:141). The previous wait_turn blocked on SpawnCollector.Wait
// for up to 5 minutes, breaking steering for the whole window. poll_turn
// pulls (TryResult, non-blocking) so the event loop keeps iterating and
// steering stays responsive.
//
// Delivery model: the child's PendingResult lands in the SpawnBus (injected
// by Router as a progress.SpawnCollector) when the child finishes. poll_turn
// uses the SpawnBusPeeper capability (optional SpawnBus method) to peek
// non-blockingly.

const pollTurnToolName = "poll_turn"

// pollTurnInput is the validated input for a poll_turn call.
type pollTurnInput struct {
	ChildTurnID string
}

// Validate checks required fields.
func (p pollTurnInput) Validate() error {
	if p.ChildTurnID == "" {
		return fmt.Errorf("child_turn_id is required")
	}
	return nil
}

// pollTurnInputSchema is the ADK input schema.
func pollTurnInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"child_turn_id": {Type: "string"},
		},
		Required:             []string{"child_turn_id"},
		AdditionalProperties: rejectAllSchema(),
	}
}

// sanitizePollTurnInput extracts {child_turn_id} from raw args and reports
// unsupported fields.
func sanitizePollTurnInput(args map[string]any) (pollTurnInput, map[string]any, []string) {
	clean := map[string]any{}
	spec := pollTurnInput{}
	var unsupported []string
	for k, v := range args {
		switch k {
		case "child_turn_id":
			s := strings.TrimSpace(fmt.Sprint(v))
			spec.ChildTurnID = s
			clean["child_turn_id"] = s
		default:
			unsupported = append(unsupported, k)
		}
	}
	return spec, clean, unsupported
}

// executePollTurn is the core logic, separable from the ADK tool wrapper for
// testing. It resolves the SpawnBusPeeper from ctx and peeks (non-blocking)
// for the child's result.
//
// Returns:
//   - {"status":"<child status>","child_turn_id":...,"result_summary":...} when done
//   - {"status":"pending","child_turn_id":...} when the child is still running
//   - {"status":"unavailable",...} when no SpawnBus / sink can't peek
//
// Never blocks, never returns an error (every outcome is a status in the map).
func executePollTurn(ctx context.Context, childID string) map[string]any {
	sink := SpawnBusFromContext(ctx)
	if sink == nil {
		return map[string]any{
			"status":         "unavailable",
			"child_turn_id":  childID,
			"error":          "no spawn result sink in context (spawn results not collectable on this turn)",
		}
	}
	peeper, ok := sink.(SpawnBusPeeper)
	if !ok {
		return map[string]any{
			"status":         "unavailable",
			"child_turn_id":  childID,
			"error":          "spawn result sink does not support polling",
		}
	}

	pr, ok := peeper.TryResult(childID)
	if !ok {
		// Child still running (or unknown ID). Tell the LLM it's pending —
		// non-blocking: the LLM can do other work and poll again later.
		return map[string]any{
			"status":        "pending",
			"child_turn_id": childID,
		}
	}

	status := pr.Result.Status
	if status == "" {
		status = "completed"
	}
	out := map[string]any{
		"status":         status,
		"child_turn_id":  childID,
		"result_summary": pr.Result.Summary,
	}
	if pr.Err != "" {
		out["error"] = pr.Err
	}
	// #68: when the child is waiting for human input, surface its question +
	// HumanRequestID so the parent LLM can decide: answer itself (answer_child
	// tool) or stay silent (escalates to the user in IM via the parent's chat
	// key). Without these the parent only sees status=waiting_human with no
	// context to act on.
	if pr.Result.Status == StatusWaitingHuman {
		if pr.Result.Question != "" {
			out["question"] = pr.Result.Question
		}
		if pr.Result.HumanRequestID != "" {
			out["human_request_id"] = pr.Result.HumanRequestID
		}
	}
	return out
}
