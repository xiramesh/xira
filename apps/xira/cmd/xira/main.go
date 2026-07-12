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
	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/channelrunner"
	"github.com/xiramesh/xira/internal/channelrunner/ingest"
	"github.com/xiramesh/xira/internal/humanrequest"
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
	cmd := &cobra.Command{
		Use:     "xira",
		Short:   "Xira customer delivery runtime",
		Version: version.String(),
	}
	cmd.SetVersionTemplate(version.String() + "\n")
	cmd.PersistentFlags().StringVar(&configPath, "config", "xira.yaml", "Runtime instance config path")
	newRuntime := func() (*runtime.Service, error) {
		cfg := runtime.Config{
			ConfigPath: configPath,
		}
		// Session storage is shared by Runtime and channel ingestion, so the
		// composition root owns construction and injects one instance into both.
		sessionManager, err := runtime.NewSessionManager(cfg)
		if err != nil {
			return nil, err
		}
		cfg.SessionManager = sessionManager
		return serviceFactory(cfg)
	}
	cmd.AddCommand(versionCommand())
	cmd.AddCommand(serveCommand(newRuntime))
	cmd.AddCommand(agentCommand(newRuntime))
	cmd.AddCommand(runsCommand(newRuntime))
	cmd.AddCommand(humanCommand(newRuntime))
	cmd.AddCommand(flowCommand(newRuntime))
	return cmd
}

func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print Xira version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), version.String())
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
				"state_dir", status["state_dir"],
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
					"allow_roots", summary.AllowRoots,
					"readonly_roots", summary.ReadonlyRoots,
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
			// #151: Inject all shared capabilities BEFORE Start (消除启动窗口）。
			rt.SetOutboundEmitter(channelRunners)
			channelRunners.SetHITLResolver(rt)
			channelRunners.SetOwnerResolver(rt)
			// #151: 创建共享 ingest 层，注入所有 runner。
			// ingest 统一处理 gate（授权+mention）/ dedupe / observe。
			ing := ingest.New(rt.SessionManager(), rt)
			channelRunners.SetIngest(ing)
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
			resp, err := rt.RunAgent(cmd.Context(), runtime.TurnRequest{AgentID: agentID, Message: message, Context: channel.NewInboundContext("cli", "", nil)})
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

func humanCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "human", Short: "Inspect and resolve human requests"}
	var status string
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List human requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			list, err := rt.ListHumanRequests(cmd.Context(), humanrequest.RequestStatus(strings.TrimSpace(status)))
			if err != nil {
				return err
			}
			return printJSON(cmd, list)
		},
	})
	cmd.Commands()[0].Flags().StringVar(&status, "status", "", "Filter by status: pending or resolved")

	cmd.AddCommand(&cobra.Command{
		Use:   "show [request-id]",
		Short: "Show a human request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			req, err := rt.GetHumanRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd, req)
		},
	})

	cmd.AddCommand(humanResolveCommand(newRuntime, "approve", humanrequest.ResponseApprove, false))
	cmd.AddCommand(humanResolveCommand(newRuntime, "deny", humanrequest.ResponseDeny, false))
	cmd.AddCommand(humanResolveCommand(newRuntime, "cancel", humanrequest.ResponseCancel, false))
	cmd.AddCommand(humanResolveCommand(newRuntime, "answer", humanrequest.ResponseAnswer, true))
	return cmd
}

func humanResolveCommand(newRuntime func() (*runtime.Service, error), name string, kind humanrequest.ResponseKind, requireMessage bool) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:   name + " [request-id]",
		Short: strings.Title(name) + " a human request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			req, err := rt.ResolveHumanRequest(cmd.Context(), args[0], humanrequest.ResolveRequest{
				Kind:    kind,
				Actor:   "cli",
				Message: message,
			})
			if err != nil {
				return err
			}
			return printJSON(cmd, req)
		},
	}
	cmd.Flags().StringVar(&message, "message", "", "Response message")
	if requireMessage {
		_ = cmd.MarkFlagRequired("message")
	}
	return cmd
}

func optionalFloat32(value *float32) any {
	if value == nil {
		return nil
	}
	return *value
}

func flowCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "flow", Short: "Run and inspect Xira flow runs"}
	cmd.AddCommand(flowRunCommand(newRuntime))
	cmd.AddCommand(flowListCommand(newRuntime))
	cmd.AddCommand(flowInspectCommand(newRuntime))
	cmd.AddCommand(flowStatusCommand(newRuntime))
	cmd.AddCommand(flowAdvanceCommand(newRuntime))
	cmd.AddCommand(flowResumeCommand(newRuntime))
	return cmd
}

func flowRunCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	var entrypoint string
	var flowPath string
	var inputs []string
	cmd := &cobra.Command{
		Use:   "run [<flow-id>]",
		Short: "Start a new flow run from a registered flow id or an explicit flow file",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			inputMap, err := parseStringSliceFlag(inputs)
			if err != nil {
				return err
			}
			req := runtime.FlowStartRequest{
				EntrypointID: entrypoint,
				Input:        inputMap,
			}
			// Explicit --path wins and is the escape hatch for unregistered or
			// ad-hoc flow files. Otherwise the positional arg is treated as a
			// registered flow id; an unknown id errors rather than falling
			// back to a path, so mis-starts fail loudly.
			if strings.TrimSpace(flowPath) != "" {
				req.FlowPath = flowPath
			} else if len(args) == 1 {
				req.FlowID = args[0]
			} else {
				return fmt.Errorf("flow id or --path is required")
			}
			run, err := rt.StartFlow(cmd.Context(), req)
			if err != nil {
				return err
			}
			return printJSON(cmd, run)
		},
	}
	cmd.Flags().StringVar(&entrypoint, "entrypoint", "", "Flow entrypoint id")
	cmd.Flags().StringVar(&flowPath, "path", "", "Explicit flow definition file path")
	cmd.Flags().StringArrayVar(&inputs, "input", nil, "Flow input as key=value (repeatable)")
	_ = cmd.MarkFlagRequired("entrypoint")
	return cmd
}

func flowListCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List flows discovered from workspace/flows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			return printJSON(cmd, rt.FlowRefs())
		},
	}
}

func flowInspectCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <flow-id>",
		Short: "Show a flow discovered from workspace/flows",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			ref, ok := rt.FlowRegistry().Find(args[0])
			if !ok {
				return fmt.Errorf("flow %q not found", args[0])
			}
			return printJSON(cmd, ref)
		},
	}
}

func flowStatusCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <flow-run-id>",
		Short: "Show a flow run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			run, err := rt.GetFlowRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd, run)
		},
	}
	return cmd
}

func flowAdvanceCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "advance <flow-run-id>",
		Short: "Advance a flow run by one step",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			run, err := rt.AdvanceFlow(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return printJSON(cmd, run)
		},
	}
	return cmd
}

func flowResumeCommand(newRuntime func() (*runtime.Service, error)) *cobra.Command {
	var humanRequestID string
	cmd := &cobra.Command{
		Use:   "resume <flow-run-id>",
		Short: "Resume a paused flow run after a human request is resolved",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rt, err := newRuntime()
			if err != nil {
				return err
			}
			defer rt.Close()
			run, err := rt.ResumeFlow(cmd.Context(), args[0], humanRequestID)
			if err != nil {
				return err
			}
			return printJSON(cmd, run)
		},
	}
	cmd.Flags().StringVar(&humanRequestID, "human-request", "", "Resolved human request id that resumes the flow")
	_ = cmd.MarkFlagRequired("human-request")
	return cmd
}

// parseStringSliceFlag parses repeated key=value flags into a map.
func parseStringSliceFlag(values []string) (map[string]string, error) {
	out := make(map[string]string, len(values))
	for _, raw := range values {
		idx := strings.Index(raw, "=")
		if idx <= 0 {
			return nil, fmt.Errorf("invalid input %q: expected key=value", raw)
		}
		out[raw[:idx]] = raw[idx+1:]
	}
	return out, nil
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
