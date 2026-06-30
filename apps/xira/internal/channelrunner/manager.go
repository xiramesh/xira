package channelrunner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelcontrol"
	"github.com/xiramesh/xira/internal/channelrunner/feishu"
	"github.com/xiramesh/xira/internal/channelrunner/ilink"
	"github.com/xiramesh/xira/internal/channelrunner/websocket"
	"github.com/xiramesh/xira/internal/runtime"
)

type Runner interface {
	ID() string
	Channel() string
	Start(context.Context) error
	Stop(context.Context) error
}

type Manager struct {
	runners []Runner
}

func NewManager(rt *runtime.Service) (*Manager, error) {
	if rt == nil {
		return &Manager{}, nil
	}
	manager := &Manager{}
	for _, definition := range rt.Entrypoints() {
		if !definition.Enabled {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(definition.Channel)) {
		case "feishu":
			runner, err := feishu.NewRunner(definition, rt, rt.StateRoot())
			if err != nil {
				return nil, err
			}
			manager.runners = append(manager.runners, runner)
			slog.Info("channel runner registered", "id", runner.ID(), "channel", runner.Channel())
		case "ilink":
			runner, err := ilink.NewRunner(definition, rt, rt.StateRoot())
			if err != nil {
				return nil, err
			}
			manager.runners = append(manager.runners, runner)
			slog.Info("channel runner registered", "id", runner.ID(), "channel", runner.Channel())
		case "websocket":
			runner, err := websocket.NewRunner(definition, rt, rt.StateRoot())
			if err != nil {
				return nil, err
			}
			manager.runners = append(manager.runners, runner)
			slog.Info("channel runner registered", "id", runner.ID(), "channel", runner.Channel())
		default:
			return nil, fmt.Errorf("entrypoint %q enables unsupported channel runner %q", definition.ID, definition.Channel)
		}
	}
	return manager, nil
}

func (m *Manager) Start(ctx context.Context) error {
	if m == nil {
		return nil
	}
	var started []Runner
	for _, runner := range m.runners {
		slog.Info("channel runner starting", "id", runner.ID(), "channel", runner.Channel())
		if err := runner.Start(ctx); err != nil {
			_ = stopRunners(ctx, started)
			slog.Error("channel runner failed to start", "id", runner.ID(), "channel", runner.Channel(), "error", err)
			return err
		}
		slog.Info("channel runner started", "id", runner.ID(), "channel", runner.Channel())
		started = append(started, runner)
	}
	return nil
}

func (m *Manager) Stop(ctx context.Context) error {
	if m == nil {
		return nil
	}
	return stopRunners(ctx, m.runners)
}

func (m *Manager) Count() int {
	if m == nil {
		return 0
	}
	return len(m.runners)
}

// WSRunner returns the websocket Runner registered with this Manager, or nil
// if none. The api package's HTTP upgrade handler delegates per-connection
// work to this Runner (RFC chatkey-session Step 3a). If multiple websocket
// entrypoints exist, the first registered wins (sufficient today; per-
// entrypoint selection can be added when needed).
func (m *Manager) WSRunner() *websocket.Runner {
	if m == nil {
		return nil
	}
	for _, runner := range m.runners {
		if ws, ok := runner.(*websocket.Runner); ok {
			return ws
		}
	}
	return nil
}

// SetHITLResolver injects the HITL resolve capability into all channel runners
// (feishu, ilink, websocket). Called by main.go after NewManager (#92 — HITL IM
// direct answer). All three channels use the same shared TryResolveHITL logic.
func (m *Manager) SetHITLResolver(resolver runtime.HITLResolver) {
	if m == nil {
		return
	}
	for _, runner := range m.runners {
		switch r := runner.(type) {
		case *feishu.Runner:
			r.SetHITLResolver(resolver)
		case *ilink.Runner:
			r.SetHITLResolver(resolver)
		case *websocket.Runner:
			r.SetHITLResolver(resolver)
		}
	}
}

func (m *Manager) CreatePairing(ctx context.Context, entrypointID string) (channelcontrol.PairingSnapshot, error) {
	controller, err := m.pairingController(entrypointID)
	if err != nil {
		return channelcontrol.PairingSnapshot{}, err
	}
	return controller.CreatePairing(ctx)
}

func (m *Manager) GetPairing(entrypointID, pairingID string) (channelcontrol.PairingSnapshot, error) {
	controller, err := m.pairingController(entrypointID)
	if err != nil {
		return channelcontrol.PairingSnapshot{}, err
	}
	return controller.GetPairing(pairingID)
}

func (m *Manager) ListAccounts(entrypointID string) ([]channelcontrol.AccountSnapshot, error) {
	controller, err := m.pairingController(entrypointID)
	if err != nil {
		return nil, err
	}
	return controller.ListAccounts()
}

func (m *Manager) DeleteAccount(ctx context.Context, entrypointID, accountID string) error {
	controller, err := m.pairingController(entrypointID)
	if err != nil {
		return err
	}
	return controller.DeleteAccount(ctx, accountID)
}

func (m *Manager) pairingController(entrypointID string) (channelcontrol.PairingController, error) {
	entrypointID = strings.TrimSpace(entrypointID)
	if entrypointID == "" {
		return nil, fmt.Errorf("entrypoint id is required")
	}
	if m == nil {
		return nil, fmt.Errorf("entrypoint %q is not running", entrypointID)
	}
	for _, runner := range m.runners {
		if runner.ID() != entrypointID {
			continue
		}
		controller, ok := runner.(channelcontrol.PairingController)
		if !ok {
			return nil, fmt.Errorf("entrypoint %q does not support runtime pairing", entrypointID)
		}
		return controller, nil
	}
	return nil, fmt.Errorf("entrypoint %q is not running", entrypointID)
}

// Emit routes an OutboundEnvelope to the runner whose Channel() matches
// envelope.Target.Channel and delegates to its Emit. This makes Manager an
// OutboundEmitter so the runtime (resume path) can deliver final responses
// back to the originating IM channel without knowing about individual runners
// (RFC #27 — stateless HITL resume).
//
// Manager.Emit is the unified outbound surface: callers (resume, future
// proactive messaging) construct an envelope with Target.Channel/ChatID, and
// Manager finds the right channel runner to actually send. Runners must
// implement channel.OutboundEmitter for their channel to be reachable.
func (m *Manager) Emit(ctx context.Context, env channel.OutboundEnvelope) error {
	if m == nil {
		return fmt.Errorf("channel manager is not configured (no runners)")
	}
	target := ""
	if env.Target != nil {
		target = strings.TrimSpace(env.Target.Channel)
	}
	if target == "" {
		return fmt.Errorf("outbound envelope has no target channel")
	}
	for _, runner := range m.runners {
		if !strings.EqualFold(runner.Channel(), target) {
			continue
		}
		emitter, ok := runner.(channel.OutboundEmitter)
		if !ok {
			return fmt.Errorf("runner %q (channel %q) does not implement OutboundEmitter", runner.ID(), runner.Channel())
		}
		return emitter.Emit(ctx, env)
	}
	return fmt.Errorf("no runner registered for channel %q", target)
}

// Capabilities returns the union of all runners' capabilities. Manager
// satisfies channel.OutboundEmitter; Capabilities advertises what the fleet
// can do (e.g. proactive_outbound for resume delivery).
func (m *Manager) Capabilities() channel.CapabilitySet {
	if m == nil {
		return nil
	}
	seen := map[channel.Capability]struct{}{}
	out := make(channel.CapabilitySet, 0)
	for _, runner := range m.runners {
		emitter, ok := runner.(channel.OutboundEmitter)
		if !ok {
			continue
		}
		for _, cap := range emitter.Capabilities() {
			if _, exists := seen[cap]; exists {
				continue
			}
			seen[cap] = struct{}{}
			out = append(out, cap)
		}
	}
	return out
}

// Compile-time: *Manager implements channel.OutboundEmitter.
var _ channel.OutboundEmitter = (*Manager)(nil)

func stopRunners(ctx context.Context, runners []Runner) error {
	var firstErr error
	for i := len(runners) - 1; i >= 0; i-- {
		slog.Info("channel runner stopping", "id", runners[i].ID(), "channel", runners[i].Channel())
		if err := runners[i].Stop(ctx); err != nil {
			slog.Error("channel runner failed to stop", "id", runners[i].ID(), "channel", runners[i].Channel(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		slog.Info("channel runner stopped", "id", runners[i].ID(), "channel", runners[i].Channel())
	}
	return firstErr
}
