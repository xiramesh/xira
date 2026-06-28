package progress

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/runtime"
)

// chatkey_session.go: per-chatKey turn engine. Extracted 1:1 from ilink's
// runTurn closure (ilink/runner.go:630-791, pre-Step-1). This is Step 1 of
// RFC xira-chatkey-session-engine-rfc-v0: pure extraction, behavior
// unchanged. The three-state machine (active/idle/hitl-paused) and the
// HandleOutcome return value land in Step 3.
//
// Why this object exists: ilink, feishu, and websocket each had their own
// turn-handling today, and only ilink had the full side-effect treatment
// (steering retry, spawn-child cancel, SpawnCollector cleanup, ChatContext
// lifecycle, dedupe complete). feishu/ws were missing pieces — a direct
// violation of per-chat-key RFC #48's single-active-turn contract. Step 1
// lifts ilink's closure into a shared object so Step 2/3 can wire feishu/ws
// to the SAME engine instead of re-implementing it.

// Deliverer sends a message (progress or final) to the channel. It is a func
// type (not an interface) so channel runners close over their own concrete
// types — ilink's *accountPoller + openilink.WeixinMessage, feishu's card
// builder, websocket's frame writer — without progress importing any of them.
//
// Named Deliverer (not Sender) to avoid collision with the pre-existing
// progress.Sender interface (chat-event SendProgress).
type Deliverer func(ctx context.Context, text string) error

// ChatKeySessionConfig holds the per-turn inputs that vary per inbound
// message but are channel-agnostic. Channel-specific delivery is injected
// via the Deliverer closures, which close over the channel's own types.
type ChatKeySessionConfig struct {
	// Runtime is the agent-run entry point. *runtime.Service satisfies this
	// implicitly; tests pass a fake.
	Runtime runtime.Runtime
	// EntrypointID is the entrypoint that owns this turn (r.definition.ID in
	// ilink). Becomes TurnRequest.EntrypointID.
	EntrypointID string
	// Inbound is the per-message channel context (channel/chat/sender).
	// Becomes TurnRequest.Context.
	Inbound channel.InboundContext

	// SendProgress delivers in-turn progress text to the channel. Wired into
	// ChatContext (the EventBus sink). May be invoked zero or many times per
	// turn depending on events.
	SendProgress Deliverer
	// SendFinal delivers the turn's final response to the channel. Invoked
	// exactly once on successful completion with a non-empty FinalResponse.
	SendFinal Deliverer

	// DedupeComplete is called on EVERY turn exit (success, error, empty
	// final) via defer — mirrors ilink's `defer messages.Complete(...)`.
	// Optional: nil = skip (feishu/ws may wire dedupe differently in Step 2/3).
	DedupeComplete func()

	// SpawnResetter clears the per-chatKey SpawnCollector on turn end,
	// preventing child-turn PendingResults from accumulating across turns
	// (memory leak in long chats). Mirrors ilink's
	// `defer router.SpawnCollectorFor(key).Reset()`. Optional: nil = skip.
	SpawnResetter func()

	// LogFields supplies channel-specific slog fields (chat_id, message_id,
	// account_id, etc.) so the Session's log lines match the channel's
	// observability. nil = no extra fields.
	LogFields []any

	// DedupeForget releases the dedupe slot on turn FAILURE (RunAgent error
	// or SendFinal failure), allowing the channel to retry the message.
	// Mirrors feishu's `messages.Forget(key)` on failure paths. When non-nil,
	// runTurn calls this instead of DedupeComplete on failure; when nil,
	// failure falls back to DedupeComplete (ilink's unconditional-complete
	// behavior — unchanged for Step 1 compatibility).
	DedupeForget func()

	// OnRunError is invoked when RunAgent returns a non-steering error.
	// Reserved extension point: the error cannot propagate (runTurn has no
	// return value per OnNewTurnFunc; turns run async in a router goroutine
	// while the channel handler has already returned, so there is nowhere to
	// surface the error to the SDK). Today feishu wires this to an slog.Error
	// call to preserve its existing error log line; ilink leaves it nil.
	// Future channels may use it for metrics/alerting/observability hooks.
	OnRunError func(err error)

	// OnTurnResult is the structured-output callback (WS, future Discord embeds).
	// When non-nil, runTurn takes the structured-output path: it SKIPS ChatContext
	// (SendProgress/SendFinal unused — the channel assembles its own output from
	// the full TurnResponse + Events). When nil (IM case: ilink/feishu), runTurn
	// uses the text-output path (ChatContext → SendProgress/SendFinal) as before.
	//
	// This is NOT a WS special-case. It's the channelrunner abstraction's
	// acknowledgment that output forms differ (feishu card, ilink text, ws frame).
	// Output was always meant to be channel-injected (RFC §5.1: "r.send = 专属").
	// WS doesn't render events to text — it streams the raw RuntimeEvents as
	// frames — so it needs the structured response, not the ChatContext sink.
	OnTurnResult func(resp runtime.TurnResponse, err error)

	// OnRawEvent is the raw-event passthrough (RFC chatkey-session). When
	// non-nil, runTurn/runTurnStructured inject a RawEventSink into the turn
	// context, and dispatchEvent delivers each in-flight signal RuntimeEvent
	// (flat, with scope/payload) to it — unrendered. The channel decides how to
	// present it: ilink/feishu wire IMEventRenderer (localized text + quota +
	// dedup, behavior-equivalent to the old ChatContext); ws wires its own
	// frame-writer; future channels can render to emoji/cards/whatever.
	//
	// This replaces ChatContext's forced render: channelrunner only passes the
	// raw event (information); the channel decides rendering (interaction). nil
	// = no raw sink (the channel gets no in-flight events — degenerate; every
	// channel should inject something, but nil is tolerated).
	OnRawEvent func(evt runtime.RuntimeEvent)

	// OnTurnEnd is invoked at turn exit (any path: success, error, steer
	// exhaustion) via defer, AFTER SpawnResetter/childCancels. Channels use it
	// to flush + release per-turn resources like IMEventRenderer's sendLoop
	// (mirrors ChatContext.Stop's "drain + wait" contract). nil = skip.
	OnTurnEnd func()
}

// ChatKeySession orchestrates one chatKey's turns. It wraps a Router
// (unchanged: owns active-routing + TTL prune) and layers the turn
// side-effects on top. One Session per chatKey, reused across turns
// (mirrors Router's entry reuse).
//
// Lifecycle is governed by the Router: Handle delegates to router.Handle,
// which prunes idle entries after routerEntryTTL (1h heuristic).
type ChatKeySession struct {
	key    runtime.ChatKey
	router *Router
	cfg    ChatKeySessionConfig
}

// NewChatKeySession constructs a Session. router may be nil only in tests
// that want to skip active-routing (Handle then runs the turn inline).
func NewChatKeySession(key runtime.ChatKey, router *Router, cfg ChatKeySessionConfig) *ChatKeySession {
	return &ChatKeySession{key: key, router: router, cfg: cfg}
}

// Handle routes + starts immediately (for ilink/feishu: pre-turn registration
// is done before this call). requestID is recorded so steered messages can cite
// it. Returns true if steered, false if started.
func (s *ChatKeySession) Handle(ctx context.Context, requestID, msg string) bool {
	if s.router != nil {
		return s.router.Handle(s.key, requestID, msg, ctx, s.runTurn)
	}
	s.runTurn(s.key, msg, ctx)
	return false
}

// Route routes WITHOUT starting the turn goroutine, returning the outcome so
// websocket can complete pre-turn registration (addActive + accepted ack)
// before calling Start(). This closes the round-5 frame-ordering gap: the turn
// cannot produce a terminal frame until the accepted ack has been sent.
func (s *ChatKeySession) Route(ctx context.Context, requestID, msg string) RoutingOutcome {
	if s.router != nil {
		return s.router.Route(s.key, requestID, msg, ctx, s.runTurn)
	}
	s.runTurn(s.key, msg, ctx)
	return RoutingOutcome{}
}

// runTurn is the extracted ilink closure body. Its signature matches
// OnNewTurnFunc exactly (`func(key, msg, ctx)`, no return) because Router
// is its only caller and that contract is fixed. Every defer, slog line,
// and ErrSteered branch carries over 1:1 from ilink/runner.go:630-791.
//
// Side-effect ordering on exit (all via defer, LIFO):
//  1. SpawnResetter (clears SpawnCollector)
//  2. childCancels.CancelAll + Reset (cancel outstanding spawned children)
//  3. chatCtx.Stop (flush progress sink)
//  4. DedupeComplete (release dedupe slot)
func (s *ChatKeySession) runTurn(_ runtime.ChatKey, turnMsg string, turnCtx context.Context) {
	key := s.key
	fields := s.cfg.LogFields

	// Raw-event passthrough (RFC chatkey-session): if the channel injected
	// OnRawEvent, wire it as a RawEventSink on the turn context. dispatchEvent
	// will then deliver each in-flight signal RuntimeEvent to it (unrendered),
	// IN PARALLEL to the EventBus/ChatContext delivery below. A channel opts
	// into raw OR text rendering — typically not both. nil = no raw sink.
	if s.cfg.OnRawEvent != nil {
		turnCtx = runtime.WithRawEventSink(turnCtx, runtime.RawEventSinkFunc(s.cfg.OnRawEvent))
	}

	// Dedupe release on turn exit. Step 2: success/failure-aware.
	// turnSucceeded is set true ONLY when the turn produced a deliverable
	// outcome (final sent, OR empty-final which is an intentional agent
	// choice to stay silent — feishu's existing semantics count that as
	// "processed"). On failure (RunAgent error or SendFinal error), if the
	// channel wired DedupeForget, that wins (lets the channel retry); else
	// fall back to DedupeComplete (ilink's unconditional-complete, Step 1).
	turnSucceeded := false
	defer func() {
		if !turnSucceeded && s.cfg.DedupeForget != nil {
			s.cfg.DedupeForget()
			return
		}
		if s.cfg.DedupeComplete != nil {
			s.cfg.DedupeComplete()
		}
	}()

	// Per-chat-key progress delivery: ChatContext renders events and calls
	// SendProgress. Created per turn (same lifetime as the steering retry loop).
	policy := DefaultPolicy()
	chatCtx := NewChatContext(turnCtx, ChatContextConfig{
		Sender:   SenderFunc(func(ctx context.Context, m Message) error {
			return s.cfg.SendProgress(ctx, m.Text)
		}),
		MaxChars: policy.MaxChars,
		Policy:   policy,
	})
	chatCtx.Start()
	defer chatCtx.Stop()

	// Per-chat-key registry of spawned-child cancel funcs (RFC #67): when
	// this turn is steered, CancelAll(chatKey) cancels every outstanding
	// child so they stop burning tokens. Created per turn. On turn-end (ANY
	// exit — steer, normal completion, or error) we CancelAll outstanding
	// children so a parent that finishes (or fails) without polling its
	// children does not leave them burning tokens to timeout. Then Reset
	// clears the registry to prevent leaks across turns.
	childCancels := NewChildCancelRegistry()
	defer func() {
		if n := childCancels.CancelAll(key); n > 0 {
			slog.Info("chatkey session turn end: canceled outstanding spawned children",
				append([]any{"chat_key", key.String(), "children", n}, fields...)...)
		}
		childCancels.Reset(key)
	}()

	// Clear spawn results when the turn ends: SpawnCollector is per-chatKey
	// (router reuses the entry across turns), and the only other cleanup
	// point is steering-retry Reset. Without this, every spawned child's
	// PendingResult accumulates in the map forever — a real memory leak in
	// long chats. A late Deliver after Reset is harmless.
	if s.cfg.SpawnResetter != nil {
		defer s.cfg.SpawnResetter()
	}

	// OnTurnEnd: flush + release per-turn channel resources (e.g. the
	// IMEventRenderer sendLoop). Deferred before the branch so BOTH the text
	// and structured paths run it at exit. nil = skip.
	if s.cfg.OnTurnEnd != nil {
		defer s.cfg.OnTurnEnd()
	}

	// Branch on output form: structured (WS/Discord) vs text (IM/ilink/feishu).
	// The shared defers above (dedupe, childCancels, SpawnResetter) apply to
	// both paths. The structured path skips ChatContext entirely — WS streams
	// raw RuntimeEvents as frames, doesn't render them to text via SendProgress.
	if s.cfg.OnTurnResult != nil {
		s.runTurnStructured(key, turnMsg, turnCtx, childCancels, fields, &turnSucceeded)
		return
	}

	// Steering retry loop: if RunAgent is canceled by steering checkpoint
	// (user interjected mid-turn), drain the SteeringQueue and re-run with
	// the interjection as the new message. Loop until RunAgent completes
	// normally or errors non-steering.
	currentMsg := turnMsg
	var resp runtime.TurnResponse
	var err error
	// Wire the EventBus (ChatContext text path) ONLY when the channel did NOT
	// opt into raw passthrough. When OnRawEvent is set, the RawEventSink (wired
	// at the top of runTurn) handles events; wiring ChatContext's EventBus too
	// would double-deliver (and ChatContext's SendProgress is nil for raw-path
	// channels anyway). Legacy text-only channels (none in-tree after the
	// ilink/feishu migration, but the path is preserved) keep ChatContext.
	wireEventBus := s.cfg.OnRawEvent == nil
	for {
		runCtx := turnCtx
		if wireEventBus {
			runCtx = runtime.WithEventBus(turnCtx, chatCtx)
		}
		// Inject the per-chat-key child cancel registry so spawned children
		// register their cancel funcs and can be canceled on steer.
		runCtx = runtime.WithChildCancelRegistry(runCtx, childCancels)
		resp, err = s.cfg.Runtime.RunAgent(runCtx, runtime.TurnRequest{
			EntrypointID: s.cfg.EntrypointID,
			Message:      currentMsg,
			Context:      s.cfg.Inbound,
		})
		// If steered (checkpoint detected pending interjection), drain queue
		// and re-run with the interjection. Uses ErrSteered sentinel (NOT
		// context.Canceled — checkpoint doesn't cancel ctx).
		if err != nil && errors.Is(err, runtime.ErrSteered) {
			if sink := runtime.SteeringBusFromContext(turnCtx); sink != nil {
				if steered, ok := sink.TryDequeue(); ok {
					// Cancel every outstanding spawned child: the user
					// interjected, so the children of the interrupted run
					// should stop rather than keep burning tokens.
					if n := childCancels.CancelAll(key); n > 0 {
						slog.Info("chatkey session steering: canceled outstanding spawned children",
							append([]any{"chat_key", key.String(), "children", n}, fields...)...)
					}
					// Reset ChatContext quota/dedup for the retried run
					// (without this, progress is silently dropped because
					// progressSent already hit cap).
					chatCtx.Reset()
					// Reset spawn results too: the retried turn must not
					// surface the previous run's stale child results.
					if s.cfg.SpawnResetter != nil {
						s.cfg.SpawnResetter()
					}
					slog.Info("chatkey session steering: restarting turn with user interjection",
						append([]any{"chat_key", key.String(), "interjection_chars", utf8.RuneCountInString(steered)}, fields...)...)
					currentMsg = steered
					continue // retry with interjection
				}
			}
		}
		break // normal completion, non-steering error, or no more interjections
	}
	if err != nil {
		if s.cfg.OnRunError != nil {
			s.cfg.OnRunError(err)
		}
		slog.Error("chatkey session runtime run failed",
			append([]any{"chat_key", key.String(), "entrypoint_id", s.cfg.EntrypointID, "error", err}, fields...)...)
		return
	}
	slog.Info("chatkey session runtime run completed",
		append([]any{
			"chat_key", key.String(),
			"entrypoint_id", s.cfg.EntrypointID,
			"run_id", resp.RunID,
			"agent_id", resp.AgentID,
			"status", resp.Status,
			"session_id", resp.SessionID,
			"tool_calls", len(resp.ToolCalls),
			"events", len(resp.Events),
			"final_response_chars", utf8.RuneCountInString(resp.FinalResponse),
		}, fields...)...)
	if strings.TrimSpace(resp.FinalResponse) == "" {
		// Empty final is an intentional agent choice to stay silent — count
		// as "processed" (matches feishu's messageProcessed=true on empty
		// final → dedupe Complete, not Forget). The message should NOT be
		// retried: re-running would just reproduce the silence.
		turnSucceeded = true
		slog.Warn("chatkey session response skipped",
			append([]any{
				"chat_key", key.String(),
				"entrypoint_id", s.cfg.EntrypointID,
				"run_id", resp.RunID,
				"reason", "empty_final_response",
			}, fields...)...)
		return
	}
	if err := s.cfg.SendFinal(turnCtx, resp.FinalResponse); err != nil {
		slog.Error("chatkey session response send failed",
			append([]any{
				"chat_key", key.String(),
				"entrypoint_id", s.cfg.EntrypointID,
				"run_id", resp.RunID,
				"error", err,
			}, fields...)...)
		return
	}
	turnSucceeded = true
	slog.Info("chatkey session response sent",
		append([]any{
			"chat_key", key.String(),
			"entrypoint_id", s.cfg.EntrypointID,
			"run_id", resp.RunID,
			"final_response_chars", utf8.RuneCountInString(resp.FinalResponse),
		}, fields...)...)
}

// runTurnStructured is the WS / structured-output turn path. Mirrors runTurn's
// text path (steering retry loop, child-cancel registry wiring) but:
//   - Does NOT create a ChatContext (WS streams raw RuntimeEvents, no text
//     rendering) and does NOT call SendProgress/SendFinal.
//   - Does NOT wire EventBus onto the run ctx (WS reads resp.Events directly
//     after RunAgent returns, not via a sink).
//   - Hands the final TurnResponse + error to OnTurnResult, which the channel
//     uses to assemble its own structured output (WS frames, future embeds).
//
// turnSucceeded is shared with runTurn's deferred dedupe logic: set true when
// OnTurnResult is invoked WITHOUT a non-steering error (i.e. the turn produced
// a result the channel can deliver). On RunAgent error, OnRunError fires first
// (if wired) and turnSucceeded stays false → DedupeForget path (if wired).
//
// (RFC chatkey-session Step 3a. See chatkey_session.go header for why the
// branch exists rather than forcing WS into the text model.)
func (s *ChatKeySession) runTurnStructured(
	key runtime.ChatKey,
	turnMsg string,
	turnCtx context.Context,
	childCancels *ChildCancelRegistry,
	fields []any,
	turnSucceeded *bool,
) {
	// Raw-event passthrough: same as the text path. WS injects OnRawEvent to
	// receive in-flight signal RuntimeEvents (which it frames itself) — this
	// complements OnTurnResult (the turn-level result). Without this, WS only
	// sees the batched resp.Events at turn end, not live progress.
	if s.cfg.OnRawEvent != nil {
		turnCtx = runtime.WithRawEventSink(turnCtx, runtime.RawEventSinkFunc(s.cfg.OnRawEvent))
	}
	currentMsg := turnMsg
	var resp runtime.TurnResponse
	var err error
	for {
		// No WithEventBus: WS consumes resp.Events directly (not via sink).
		runCtx := runtime.WithChildCancelRegistry(turnCtx, childCancels)
		resp, err = s.cfg.Runtime.RunAgent(runCtx, runtime.TurnRequest{
			EntrypointID: s.cfg.EntrypointID,
			Message:      currentMsg,
			Context:      s.cfg.Inbound,
		})
		if err != nil && errors.Is(err, runtime.ErrSteered) {
			if sink := runtime.SteeringBusFromContext(turnCtx); sink != nil {
				if steered, ok := sink.TryDequeue(); ok {
					if n := childCancels.CancelAll(key); n > 0 {
						slog.Info("chatkey session steering: canceled outstanding spawned children",
							append([]any{"chat_key", key.String(), "children", n}, fields...)...)
					}
					if s.cfg.SpawnResetter != nil {
						s.cfg.SpawnResetter()
					}
					slog.Info("chatkey session steering: restarting turn with user interjection",
						append([]any{"chat_key", key.String(), "interjection_chars", utf8.RuneCountInString(steered)}, fields...)...)
					currentMsg = steered
					continue
				}
			}
		}
		break
	}
	if err != nil {
		if s.cfg.OnRunError != nil {
			s.cfg.OnRunError(err)
		}
		slog.Error("chatkey session runtime run failed",
			append([]any{"chat_key", key.String(), "entrypoint_id", s.cfg.EntrypointID, "error", err}, fields...)...)
		// Still call OnTurnResult so WS can send an error frame to the client
		// (e.g. run_failed). turnSucceeded stays false → DedupeForget if wired.
		s.cfg.OnTurnResult(resp, err)
		return
	}
	slog.Info("chatkey session runtime run completed",
		append([]any{
			"chat_key", key.String(),
			"entrypoint_id", s.cfg.EntrypointID,
			"run_id", resp.RunID,
			"agent_id", resp.AgentID,
			"status", resp.Status,
			"session_id", resp.SessionID,
			"tool_calls", len(resp.ToolCalls),
			"events", len(resp.Events),
		}, fields...)...)
	*turnSucceeded = true
	s.cfg.OnTurnResult(resp, nil)
}
