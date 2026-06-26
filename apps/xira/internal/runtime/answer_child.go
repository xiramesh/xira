package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"

	"github.com/xiramesh/xira/internal/humanrequest"
)

// answer_child.go: the #68 tool a parent LLM uses to answer a spawned child's
// HITL question on the child's behalf. The flow:
//
//	child calls human.request → child turn enters waiting_human, persists,
//	spawn goroutine delivers a PendingResult with Question + HumanRequestID →
//	parent's poll_turn surfaces the question → parent LLM decides to answer
//	itself (answer_child) OR stay silent (escalates to the user in IM).
//
// answer_child resolves the child's HumanRequest with the parent's answer,
// which resumes the child turn in the background.
//
// # CRITICAL: non-blocking
//
// Resolving a child's HumanRequest triggers resumeDirectHumanRequest → a full
// s.generate (a child LLM run), which is heavy and synchronous. Running it in
// the parent's tool handler would freeze the parent's event loop and disable
// the steering checkpoint — the same death that retired wait_turn (PR #53; see
// poll_turn.go:17-23 and spawn_bus.go:30-34). So executeAnswerChild launches
// the resolve in a DETACHED goroutine on a context.Background() base (NOT the
// parent ctx — the parent turn ending must NOT cancel the child's resume), and
// returns {status:"answering"} immediately. The child's resumed final then
// routes back to the parent's chat key (断裂 A fix).

const answerChildToolName = "answer_child"

// answerChildInput is the parent-facing args: which child question to answer,
// and the answer.
type answerChildInput struct {
	HumanRequestID string // surfaced by poll_turn when the child is waiting_human
	Answer         string // the parent's answer to the child's question
}

func (in answerChildInput) Validate() error {
	if strings.TrimSpace(in.HumanRequestID) == "" {
		return fmt.Errorf("human_request_id is required (the id poll_turn surfaced)")
	}
	if strings.TrimSpace(in.Answer) == "" {
		return fmt.Errorf("answer is required")
	}
	return nil
}

// answerChildInputSchema is the ADK input schema.
func answerChildInputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"human_request_id": {Type: "string", Description: "The id poll_turn surfaced when the child entered waiting_human."},
			"answer":           {Type: "string", Description: "Your answer to the child's question. The child resumes with this as the human response."},
		},
		Required:             []string{"human_request_id", "answer"},
		AdditionalProperties: rejectAllSchema(),
	}
}

// sanitizeAnswerChildInput extracts {human_request_id, answer} from raw args
// and reports unsupported fields.
func sanitizeAnswerChildInput(args map[string]any) (answerChildInput, map[string]any, []string) {
	clean := map[string]any{}
	spec := answerChildInput{}
	var unsupported []string
	for k, v := range args {
		switch k {
		case "human_request_id":
			s := strings.TrimSpace(fmt.Sprint(v))
			spec.HumanRequestID = s
			clean["human_request_id"] = s
		case "answer":
			s := strings.TrimSpace(fmt.Sprint(v))
			spec.Answer = s
			clean["answer"] = s
		default:
			unsupported = append(unsupported, k)
		}
	}
	return spec, clean, unsupported
}

// executeAnswerChild is the core logic, separable from the ADK tool wrapper for
// testing. It resolves the child's HumanRequest asynchronously (in a detached
// goroutine) and returns {status:"answering"} immediately — never blocking.
//
// Errors (bad input, unknown HumanRequest) are returned synchronously as
// {status:"rejected", error:...}. The async resume failure (LLM error etc.) is
// logged and surfaces via the child run's status, not via this tool's return.
func executeAnswerChild(_ context.Context, s *Service, in answerChildInput) map[string]any {
	if err := in.Validate(); err != nil {
		return map[string]any{"status": "rejected", "error": err.Error()}
	}
	// Verify the request exists + is resolvable BEFORE going async, so the
	// parent LLM gets synchronous feedback for a bad id (not a silent async
	// failure). We do NOT resolve here — that's async below.
	existing, err := s.GetHumanRequest(context.Background(), in.HumanRequestID)
	if err != nil || existing == nil {
		return map[string]any{"status": "rejected", "error": fmt.Sprintf("human request %q not found", in.HumanRequestID)}
	}
	if existing.Status != humanrequest.StatusPending {
		return map[string]any{"status": "rejected", "error": fmt.Sprintf("human request %q is already %s", in.HumanRequestID, existing.Status)}
	}

	requestID := in.HumanRequestID
	answer := in.Answer
	resolve := func() {
		// context.Background(): the parent turn may end right after this tool
		// returns; the child's resume must outlive it. Matches spawnCore's
		// deliberate Background() base (spawn_turn.go:108-118).
		defer func() {
			if r := recover(); r != nil {
				slog.Error("answer_child async resolve panicked",
					"human_request_id", requestID,
					"panic", r)
			}
		}()
		_, rerr := s.ResolveHumanRequest(context.Background(), requestID, humanrequest.ResolveRequest{
			Kind:    humanrequest.ResponseAnswer,
			Actor:   "parent_agent",
			Message: answer,
		})
		if rerr != nil {
			// The HumanRequest is already resolved (store-level, before the
			// resume run). A resume model-call error surfaces via the child
			// run's status; we log it, not swallow it.
			slog.Warn("answer_child async resolve finished with error",
				"human_request_id", requestID,
				"error", rerr)
		}
	}
	go resolve()

	return map[string]any{
		"status":           "answering",
		"human_request_id": requestID,
		"note":             "child turn resumed in the background; its result routes back to this chat when done",
	}
}

// newAnswerChildCallID returns a tool-call id for the record (matching the
// spawn_turn/poll_turn record id pattern).
func newAnswerChildCallID() string {
	return "answer_child:" + uuid.NewString()
}

// _ keeps the time import if the time.Now() usage in the ADK wrapper moves.
var _ = time.Now
