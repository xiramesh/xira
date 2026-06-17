package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const defaultCommandTimeout = 60 * time.Second
const defaultShellTimeout = 30 * time.Second

type CommandRunTool struct {
	workspaceRoot  string
	writeRoots     []string
	defaultTimeout time.Duration
}

type ShellRunTool struct {
	workspaceRoot  string
	writeRoots     []string
	defaultTimeout time.Duration
}

func NewCommandRunTool(workspaceRoot string, writeRoots []string) *CommandRunTool {
	return &CommandRunTool{
		workspaceRoot:  cleanWorkspace(workspaceRoot),
		writeRoots:     writeRoots,
		defaultTimeout: defaultCommandTimeout,
	}
}

func NewShellRunTool(workspaceRoot string, writeRoots []string) *ShellRunTool {
	return &ShellRunTool{
		workspaceRoot:  cleanWorkspace(workspaceRoot),
		writeRoots:     writeRoots,
		defaultTimeout: defaultShellTimeout,
	}
}

func (t *CommandRunTool) Name() string { return "command.run" }
func (t *CommandRunTool) Description() string {
	return "Run one local program with structured argv in the Xira workspace. Use this by default for commands that do not need shell pipes, redirection, command substitution, or other shell language."
}
func (t *CommandRunTool) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"program":          map[string]any{"type": "string", "description": "Executable name or path to run."},
			"args":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Program arguments. Do not include the program name."},
			"cwd":              map[string]any{"type": "string", "description": "Working directory. Relative paths resolve under the workspace."},
			"timeout_seconds":  map[string]any{"type": "integer", "description": "Timeout in seconds. Defaults to 60."},
			"max_stdout_bytes": map[string]any{"type": "integer", "description": "Maximum stdout preview bytes returned to the model."},
			"max_stderr_bytes": map[string]any{"type": "integer", "description": "Maximum stderr preview bytes returned to the model."},
		},
		"required": []string{"program"},
	}
}
func (t *CommandRunTool) Policy() ToolPolicy {
	return ToolPolicy{Risk: "medium"}
}
func (t *CommandRunTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	program, _ := args["program"].(string)
	program = strings.TrimSpace(program)
	if program == "" {
		return nil, fmt.Errorf("program is required")
	}
	if strings.ContainsAny(program, "|><;&$`\n\r") {
		return nil, fmt.Errorf("program must be a single executable path or name; use shell.run for shell syntax")
	}
	argv, err := stringSliceArg(args, "args")
	if err != nil {
		return nil, err
	}
	cwd, err := resolveToolCWD(t.workspaceRoot, t.writeRoots, mapStringArg(args, "cwd"))
	if err != nil {
		return nil, err
	}
	timeout := timeoutArg(args, t.defaultTimeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, program, argv...)
	configureCommandCancellation(cmd)
	cmd.Dir = cwd
	return runProcess(runCtx, cmd, map[string]any{
		"tool":    t.Name(),
		"program": program,
		"args":    argv,
		"cwd":     cwd,
	})
}

func (t *ShellRunTool) Name() string { return "shell.run" }
func (t *ShellRunTool) Description() string {
	return "Run a shell command in the Xira workspace. Use only when shell language is required, such as pipes, redirection, &&, command substitution, or heredocs."
}
func (t *ShellRunTool) Parameters() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"command":          map[string]any{"type": "string", "description": "Shell command to execute."},
			"cwd":              map[string]any{"type": "string", "description": "Working directory. Relative paths resolve under the workspace."},
			"timeout_seconds":  map[string]any{"type": "integer", "description": "Timeout in seconds. Defaults to 30."},
			"max_stdout_bytes": map[string]any{"type": "integer", "description": "Maximum stdout preview bytes returned to the model."},
			"max_stderr_bytes": map[string]any{"type": "integer", "description": "Maximum stderr preview bytes returned to the model."},
		},
		"required": []string{"command"},
	}
}
func (t *ShellRunTool) Policy() ToolPolicy {
	return ToolPolicy{Risk: "high"}
}
func (t *ShellRunTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	command, _ := args["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("command is required")
	}
	cwd, err := resolveToolCWD(t.workspaceRoot, t.writeRoots, mapStringArg(args, "cwd"))
	if err != nil {
		return nil, err
	}
	timeout := timeoutArg(args, t.defaultTimeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, shellArgs := shellCommand(command)
	cmd := exec.CommandContext(runCtx, shell, shellArgs...)
	configureCommandCancellation(cmd)
	cmd.Dir = cwd
	return runProcess(runCtx, cmd, map[string]any{
		"tool":    t.Name(),
		"command": command,
		"cwd":     cwd,
	})
}

func runProcess(runCtx context.Context, cmd *exec.Cmd, out map[string]any) (map[string]any, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		exitCode = -1
		err = runCtx.Err()
	}
	out["stdout"] = stdout.String()
	out["stderr"] = stderr.String()
	out["stdout_bytes"] = stdout.Len()
	out["stderr_bytes"] = stderr.Len()
	out["exit_code"] = exitCode
	out["duration_ms"] = duration.Milliseconds()
	if err != nil {
		out["error"] = err.Error()
	}
	return out, err
}

func resolveToolCWD(workspaceRoot string, writeRoots []string, rawCWD string) (string, error) {
	rawCWD = strings.TrimSpace(rawCWD)
	if rawCWD == "" {
		return workspaceRoot, nil
	}
	var cwd string
	if filepath.IsAbs(rawCWD) {
		cwd = filepath.Clean(rawCWD)
	} else {
		cwd = filepath.Clean(filepath.Join(workspaceRoot, rawCWD))
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if !pathWithinRoots(abs, writeRoots) {
		return "", fmt.Errorf("cwd must be within allowed roots")
	}
	return abs, nil
}

func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", command}
	}
	shell := strings.TrimSpace(os.Getenv("SHELL"))
	if shell == "" {
		shell = "/bin/sh"
	}
	return shell, []string{"-lc", command}
}

func timeoutArg(args map[string]any, fallback time.Duration) time.Duration {
	if raw, ok := numberArg(args, "timeout_seconds"); ok && raw > 0 {
		return time.Duration(raw) * time.Second
	}
	return fallback
}

func mapStringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, ok := args[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func stringSliceArg(args map[string]any, key string) ([]string, error) {
	if args == nil || args[key] == nil {
		return nil, nil
	}
	switch value := args[key].(type) {
	case []string:
		return append([]string(nil), value...), nil
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s must contain only strings", key)
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
}

func numberArg(args map[string]any, key string) (int, bool) {
	switch value := args[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	case jsonNumber:
		n, err := value.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

type jsonNumber interface {
	Int64() (int64, error)
}
