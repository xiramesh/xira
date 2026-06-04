package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ai-daming/xira/internal/api"
	"github.com/ai-daming/xira/internal/channelrunner"
	"github.com/ai-daming/xira/internal/runtime"
	"github.com/ai-daming/xira/internal/tui"
	"github.com/ai-daming/xira/internal/version"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	return newRootCommandWithFactory(runtime.NewService)
}

func newRootCommandWithFactory(serviceFactory func(runtime.Config) (*runtime.Service, error)) *cobra.Command {
	var configPath string
	var runRoot string
	cmd := &cobra.Command{
		Use:   "xira",
		Short: "Xira customer delivery runtime",
	}
	cmd.PersistentFlags().StringVar(&configPath, "config", "xira.yaml", "Runtime instance config path")
	cmd.PersistentFlags().StringVar(&runRoot, "run-root", "", "Override run log root directory")
	newRuntime := func() (*runtime.Service, error) {
		return serviceFactory(runtime.Config{
			ConfigPath: configPath,
			RunRoot:    runRoot,
		})
	}
	cmd.AddCommand(versionCommand())
	cmd.AddCommand(serveCommand(newRuntime))
	cmd.AddCommand(agentCommand(newRuntime))
	cmd.AddCommand(runsCommand(newRuntime))
	cmd.AddCommand(tuiCommand(newRuntime))
	return cmd
}

func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print Xira version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", version.Name, version.Version)
		},
	}
}

func serveCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run Xira runtime API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			status := rt.Status()
			slog.Info("xira serve starting",
				"addr", addr,
				"config_path", status["config_path"],
				"workspace", status["workspace"],
				"run_root", status["run_root"],
				"default_agent", status["default_agent"],
				"profile_source", status["profile_source"],
			)
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			channelRunners, err := channelrunner.NewManager(rt)
			if err != nil {
				return err
			}
			if err := channelRunners.Start(ctx); err != nil {
				return err
			}
			slog.Info("channel runners started", "count", channelRunners.Count())
			defer func() {
				stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = channelRunners.Stop(stopCtx)
				slog.Info("channel runners stopped", "count", channelRunners.Count())
			}()
			srv := api.NewServer(rt, addr)
			fmt.Fprintf(cmd.OutOrStdout(), "xira runtime listening on %s (channel runners: %d)\n", srv.URL(), channelRunners.Count())
			slog.Info("xira http server listening", "url", srv.URL(), "channel_runners", channelRunners.Count())
			if err := srv.Start(ctx); err != nil {
				slog.Error("xira http server stopped with error", "error", err)
				return err
			}
			slog.Info("xira serve stopped")
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8089", "HTTP listen address")
	return cmd
}

func agentCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Manage and run agent profiles"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List agent profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			return printJSON(cmd, rt.Agents())
		},
	})
	var agentID string
	var message string
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run an agent profile once",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			resp, err := rt.RunAgent(cmd.Context(), runtime.TurnRequest{AgentID: agentID, Message: message, Channel: "cli"})
			if printErr := printJSON(cmd, resp); printErr != nil {
				return printErr
			}
			return err
		},
	}
	runCmd.Flags().StringVar(&agentID, "agent", "", "Agent profile ID; defaults to runtime default_agent")
	runCmd.Flags().StringVar(&message, "message", "", "User message")
	_ = runCmd.MarkFlagRequired("message")
	cmd.AddCommand(runCmd)
	return cmd
}

func runsCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "runs", Short: "Inspect run logs"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			runs, err := rt.RunStore().List()
			if err != nil {
				return err
			}
			return printJSON(cmd, runs)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show [run-id]",
		Short: "Show run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			run, err := rt.RunStore().Load(args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd, run)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "export [run-id]",
		Short: "Print run as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			run, err := rt.RunStore().Load(args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd, run)
		},
	})
	return cmd
}

func tuiCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	var agentID string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Run embedded Xira TUI",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			agentID = strings.TrimSpace(agentID)
			if agentID != "" && !runtimeHasAgent(rt, agentID) {
				return fmt.Errorf("agent profile %q not found", agentID)
			}
			return tui.Run(cmd.Context(), rt, agentID)
		},
	}
	cmd.Flags().StringVar(&agentID, "agent", "", "Start TUI in agent mode with this profile ID")
	return cmd
}

func runtimeHasAgent(rt *runtime.Service, agentID string) bool {
	for _, profile := range rt.Agents() {
		if profile.ID == agentID {
			return true
		}
	}
	return false
}

func printJSON(cmd *cobra.Command, value any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
