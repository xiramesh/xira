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

	"github.com/xiramesh/xira/internal/api"
	"github.com/xiramesh/xira/internal/channelrunner"
	"github.com/xiramesh/xira/internal/runtime"
	"github.com/xiramesh/xira/internal/version"
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
				"state_root", status["state_root"],
				"default_agent", status["default_agent"],
				"profile_source", status["profile_source"],
			)
			for _, summary := range rt.AgentSummaries() {
				slog.Info("agent profile loaded",
					"agent_id", summary.AgentID,
					"provider", summary.Provider,
					"model", summary.Model,
					"stream", summary.Stream,
					"temperature", optionalFloat32(summary.Temperature),
					"thinking_type", summary.ThinkingType,
					"tools", summary.Tools,
					"profile_source", summary.ProfileSource,
					"instruction_hash", summary.InstructionHash,
				)
			}
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
			srv := api.NewServer(rt, addr, channelRunners)
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
	var outputFormat string
	var jsonOutput bool
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run an agent profile once",
		RunE: func(cmd *cobra.Command, args []string) error {
			if outputFormat != "text" && outputFormat != "json" {
				return fmt.Errorf("unsupported output format %q; expected text or json", outputFormat)
			}
			format := outputFormat
			if jsonOutput {
				format = "json"
			}
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			resp, err := rt.RunAgent(cmd.Context(), runtime.TurnRequest{AgentID: agentID, Message: message, Channel: "cli"})
			if format == "json" {
				if printErr := printJSON(cmd, resp); printErr != nil {
					return printErr
				}
			} else {
				if printErr := printFinalResponse(cmd, resp.FinalResponse); printErr != nil {
					return printErr
				}
			}
			return err
		},
	}
	runCmd.Flags().StringVar(&agentID, "agent", "", "Agent profile ID; defaults to runtime default_agent")
	runCmd.Flags().StringVar(&message, "message", "", "User message")
	runCmd.Flags().StringVar(&outputFormat, "output", "text", "Output format: text or json")
	runCmd.Flags().BoolVar(&jsonOutput, "json", false, "Print the full TurnResponse as JSON")
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

func optionalFloat32(value *float32) any {
	if value == nil {
		return nil
	}
	return *value
}

func printJSON(cmd *cobra.Command, value any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func printFinalResponse(cmd *cobra.Command, text string) error {
	if _, err := fmt.Fprint(cmd.OutOrStdout(), text); err != nil {
		return err
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		_, err := fmt.Fprintln(cmd.OutOrStdout())
		return err
	}
	return nil
}
