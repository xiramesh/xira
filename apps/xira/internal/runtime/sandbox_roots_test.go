package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiramesh/xira/internal/channel"
	"github.com/xiramesh/xira/internal/model/deepseek"
)

// sandboxProfile builds a minimal xira-assistant PROFILE.md frontmatter with the
// given tools and sandbox roots.
func sandboxProfile(tools, allowRoots, readonlyRoots []string) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("id: xira-assistant\nname: Xira Assistant\nversion: 0.1.1\n")
	b.WriteString("model_policy:\n  provider: deepseek\n  model: deepseek-v4-flash\n")
	b.WriteString("tools:\n")
	for _, name := range tools {
		b.WriteString("  - " + name + "\n")
	}
	for _, key := range []struct {
		field string
		roots []string
	}{
		{"allow_roots", allowRoots},
		{"readonly_roots", readonlyRoots},
	} {
		if len(key.roots) == 0 {
			continue
		}
		b.WriteString(key.field + ":\n")
		for _, root := range key.roots {
			b.WriteString("  - \"" + root + "\"\n")
		}
	}
	b.WriteString("verification:\n  default_checks:\n    - final_response_non_empty\n")
	b.WriteString("---\n# Working Contract\n\nUse runtime tools carefully.\n")
	return b.String()
}

func newSandboxConfirmationRuntime(t *testing.T, workspace string, client *http.Client, tools, allowRoots, readonlyRoots []string) *Service {
	t.Helper()
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "PROFILE.md"), sandboxProfile(tools, allowRoots, readonlyRoots))
	writeFile(t, filepath.Join(workspace, "agents", "xira-assistant", "SOUL.md"), "# Soul\n\nDirect.\n")
	return newTestService(t, Config{
		WorkspaceRoot:  workspace,
		StateDir:       filepath.Join(t.TempDir(), "state"),
		DeepSeekClient: deepseek.New(deepseek.WithBaseURLForTest("http://deepseek.test"), deepseek.WithAPIKey("test-key"), deepseek.WithHTTPClient(client)),
	})
}

// TestEditFileExecutesDirectlyInAllowRoot guards #110: edit_file no longer goes
// through a confirmation gate. allow_roots is a hard boundary — an in-bound
// edit executes directly (the file is modified) and does NOT produce a
// waiting_human interrupt or human request.
func TestEditFileExecutesDirectlyInAllowRoot(t *testing.T) {
	workspace := t.TempDir()
	allowDir := t.TempDir()
	targetPath := filepath.Join(allowDir, "target.txt")
	writeFile(t, targetPath, "original line")

	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		// First call: emit edit_file. After edit executes, the loop calls again
		// with a tool result — return a final to end the turn. Pre-#110 the
		// gate broke this loop by entering waiting_human; now edit completes
		// and the model must produce a final.
		var req deepseek.ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if hasToolResponse(req.Messages) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(deepSeekTextResponse("edit done"))),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(deepSeekToolCallResponseWithArgs("edit-confirm-call", "edit_file", map[string]any{
				"path": targetPath, "old_text": "original", "new_text": "changed",
			}))),
		}, nil
	})}
	rt := newSandboxConfirmationRuntime(t, workspace, client, []string{"edit_file"}, []string{allowDir}, nil)

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "edit the file", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if resp.Status == StatusWaitingHuman {
		t.Fatalf("status = waiting_human: edit_file must NOT gate in allow_roots (#110); human_requests=%+v", resp.HumanRequests)
	}
	if len(resp.HumanRequests) != 0 {
		t.Fatalf("edit_file produced a human request (%+v) — gate should be gone (#110)", resp.HumanRequests)
	}
	// In-bound edit executed directly (allow_roots boundary permits it).
	if data, _ := os.ReadFile(targetPath); !strings.Contains(string(data), "changed") {
		t.Fatalf("edit_file did not execute in-bound; content = %q", string(data))
	}
}

// TestRunSnapshotRecordsSandboxRoots guards B2: the per-run ModelPolicySnapshot
// must carry the authorized roots so a run artifact answers "which
// out-of-workspace roots was this run allowed to reach". (The
// model.policy_resolved runtime event was removed in #43 — roots are read from
// resp.ModelPolicy, the authoritative source persisted in run.json.)
func TestRunSnapshotRecordsSandboxRoots(t *testing.T) {
	workspace := t.TempDir()
	allowDir := t.TempDir()
	readonlyDir := t.TempDir()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(deepSeekTextResponse("done"))),
		}, nil
	})}
	rt := newSandboxConfirmationRuntime(t, workspace, client, []string{"read_file"}, []string{allowDir}, []string{readonlyDir})

	resp, err := rt.RunAgent(context.Background(), TurnRequest{Message: "hi", Context: channel.NewInboundContext("test", "user-1", nil)})
	if err != nil {
		t.Fatalf("RunAgent() error = %v", err)
	}
	if !containsString(resp.ModelPolicy.AllowRoots, allowDir) {
		t.Fatalf("run snapshot allow_roots = %+v, want %q", resp.ModelPolicy.AllowRoots, allowDir)
	}
	if !containsString(resp.ModelPolicy.ReadonlyRoots, readonlyDir) {
		t.Fatalf("run snapshot readonly_roots = %+v, want %q", resp.ModelPolicy.ReadonlyRoots, readonlyDir)
	}
}
