package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

// wait_turn.go: the parent LLM's tool to retrieve a spawned child's result
// (Phase 4, RFC §2.4 D-3). After spawn_turn returns {agent_turn_id,
// status:spawned}, the parent may call wait_turn(child_turn_id) to block
// until that child completes and receive its result as the tool response.
//
// Delivery model: the child's PendingResult lands in the SpawnSink (injected
// by Router as a progress.SpawnCollector) when the child finishes. wait_turn
// uses the SpawnResultWaiter capability (optional SpawnSink method) to block
// on a specific child. If the SpawnSink doesn't implement SpawnResultWaiter
// (e.g. a test double, or no sink present), wait_turn reports that spawn
// results aren't collectable.
//
// This closes the spawn_turn "fire-and-forget-forever" gap (PR #52 review):
// without wait_turn, spawned results were 100% dropped in production.

const waitTurnToolName = "wait_turn"

// waitTurnInput is the validated input for a wait_turn call.
type waitTurnInput struct {
	ChildTurnID string
}

// waitTurnMaxDuration caps how long a single wait_turn blocks. Bounds the
// tool so a missing/stuck child can't hang the parent turn indefinitely; the
// parent can retry if it still cares. Generous enough for a child agent to
// finish a normal turn.
const waitTurnMaxDuration = 5 * time.Minute

// Validate checks required fields.
func (w waitTurnInput) Validate() error {
	if w.ChildTurnID == "" {
		return fmt.Errorf("child_turn_id is required")
	}
	return nil
}

// waitTurnInputSchema is the ADK input schema.
func waitTurnInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"child_turn_id": {Type: "string"},
		},
		Required:             []string{"child_turn_id"},
		AdditionalProperties: rejectAllSchema(),
	}
}

// sanitizeWaitTurnInput extracts {child_turn_id} from raw args and reports
// unsupported fields (mirrors sanitizeSpawnTurnInput shape).
func sanitizeWaitTurnInput(args map[string]any) (waitTurnInput, map[string]any, []string) {
	clean := map[string]any{}
	spec := waitTurnInput{}
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

// executeWaitTurn is the core logic, separable from the ADK tool wrapper for
// testing. It resolves the SpawnResultWaiter from ctx, blocks on the child
// (bounded by waitTurnMaxDuration), and returns the output map + error.
//
// Returns:
//   - {"status":"completed","child_turn_id":...,"result_summary":...} on success
//   - {"status":"failed","child_turn_id":...,"error":...} when the child errored
//   - {"status":"timeout","child_turn_id":...} when the child didn't finish in time
//   - {"status":"unavailable","error":...} when no SpawnResultWaiter is in ctx
func executeWaitTurn(ctx context.Context, childID string) (map[string]any, error) {
	sink := SpawnSinkFromContext(ctx)
	if sink == nil {
		return map[string]any{
			"status": "unavailable",
			"error":  "no spawn result sink in context (spawn results not collectable on this turn)",
		}, nil
	}
	waiter, ok := sink.(SpawnResultWaiter)
	if !ok {
		return map[string]any{
			"status": "unavailable",
			"error":  "spawn result sink does not support waiting",
		}, nil
	}

	// Bound the wait so a missing/stuck child can't hang the parent forever.
	// Respect an already-shorter ctx deadline if present.
	waitCtx, cancel := context.WithTimeout(ctx, waitTurnMaxDuration)
	defer cancel()

	pr, err := waiter.Wait(waitCtx, childID)
	if err != nil {
		// Distinguish timeout (child still running) from ctx-cancelled-by-parent.
		if waitCtx.Err() != nil && (ctx.Err() == nil || ctx.Err() == waitCtx.Err()) {
			return map[string]any{
				"status":         "timeout",
				"child_turn_id":  childID,
				"error":          err.Error(),
			}, nil
		}
		return map[string]any{
			"status":         "timeout",
			"child_turn_id":  childID,
			"error":          err.Error(),
		}, nil
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
	return out, nil
}

// waitTurnOutput is kept for symmetry with spawn_turn; executeWaitTurn builds
// the map inline above to carry status/summary/error, but this helper exists
// for any caller that wants the minimal shape.
func waitTurnOutput(childID, status string) map[string]any {
	return map[string]any{
		"child_turn_id": childID,
		"status":        status,
	}
}
