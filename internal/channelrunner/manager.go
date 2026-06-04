package channelrunner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ai-daming/xira/internal/channelrunner/feishu"
	"github.com/ai-daming/xira/internal/runtime"
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
			runner, err := feishu.NewRunner(definition, rt)
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
