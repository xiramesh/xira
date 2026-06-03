package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type ExecTool struct {
	workspaceRoot  string
	defaultTimeout time.Duration
}

func NewExecTool(workspaceRoot string) *ExecTool {
	return &ExecTool{
		workspaceRoot:  cleanWorkspace(workspaceRoot),
		defaultTimeout: 60 * time.Second,
	}
}

func (t *ExecTool) Name() string { return "exec" }
func (t *ExecTool) Description() string {
	return "Execute a shell command in the FlowDeck workspace. First version supports action=run only."
}
func (t *ExecTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":          map[string]any{"type": "string", "enum": []string{"run"}, "description": "Only run is supported in FlowDeck v1."},
			"command":         map[string]any{"type": "string", "description": "Shell command to execute."},
			"cwd":             map[string]any{"type": "string", "description": "Working directory. Relative paths resolve under the workspace."},
			"timeout_seconds": map[string]any{"type": "integer", "description": "Timeout in seconds. Defaults to 60."},
		},
		"required": []string{"action", "command"},
	}
}

func (t *ExecTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	action, _ := args["action"].(string)
	if strings.TrimSpace(action) == "" {
		action = "run"
	}
	if action != "run" {
		return nil, fmt.Errorf("unsupported exec action %q", action)
	}
	command, _ := args["command"].(string)
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("command is required")
	}
	cwd := t.workspaceRoot
	if rawCWD, _ := args["cwd"].(string); strings.TrimSpace(rawCWD) != "" {
		cwd = resolveCWD(t.workspaceRoot, rawCWD)
	}
	timeout := t.defaultTimeout
	if raw, ok := numberArg(args, "timeout_seconds"); ok && raw > 0 {
		timeout = time.Duration(raw) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, shellArgs := shellCommand(command)
	cmd := exec.CommandContext(runCtx, shell, shellArgs...)
	cmd.Dir = cwd
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
	if runCtx.Err() == context.DeadlineExceeded {
		exitCode = -1
		err = runCtx.Err()
	}
	out := map[string]any{
		"action":      action,
		"command":     command,
		"cwd":         cwd,
		"stdout":      stdout.String(),
		"stderr":      stderr.String(),
		"exit_code":   exitCode,
		"duration_ms": duration.Milliseconds(),
	}
	if err != nil {
		out["error"] = err.Error()
	}
	return out, err
}

func resolveCWD(workspaceRoot, rawCWD string) string {
	rawCWD = strings.TrimSpace(rawCWD)
	if rawCWD == "" {
		return workspaceRoot
	}
	if filepath.IsAbs(rawCWD) {
		return filepath.Clean(rawCWD)
	}
	return filepath.Clean(filepath.Join(workspaceRoot, rawCWD))
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
