package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ai-daming/flowdeck/internal/api"
	"github.com/ai-daming/flowdeck/internal/runtime"
	"github.com/ai-daming/flowdeck/internal/tui"
	"github.com/ai-daming/flowdeck/internal/version"
)

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var configPath string
	var runRoot string
	var mockModel bool
	cmd := &cobra.Command{
		Use:   "flowdeck",
		Short: "FlowDeck customer delivery runtime",
	}
	cmd.PersistentFlags().StringVar(&configPath, "config", "flowdeck.yaml", "Runtime instance config path")
	cmd.PersistentFlags().StringVar(&runRoot, "run-root", "", "Override run log root directory")
	cmd.PersistentFlags().BoolVar(&mockModel, "mock-model", false, "Use mock model instead of DeepSeek")
	newRuntime := func() (*runtime.Service, error) {
		return runtime.NewService(runtime.Config{
			ConfigPath:   configPath,
			RunRoot:      runRoot,
			UseMockModel: mockModel,
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
		Short: "Print FlowDeck version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", version.Name, version.Version)
		},
	}
}

func serveCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run FlowDeck runtime API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			srv := api.NewServer(rt, addr)
			fmt.Fprintf(cmd.OutOrStdout(), "flowdeck runtime listening on %s\n", srv.URL())
			return srv.Start(ctx)
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
		Short: "Run embedded FlowDeck TUI",
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
