package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// This file enforces SEALED-interface exhaustiveness by SCANNING SOURCE,
// not by walking a hand-maintained list. PR #31 review W3: the previous
// "exhaustive" tests iterated a hardcoded slice of known types, so adding a
// new type (e.g. SneakyMessage) that satisfied the interface passed every
// test — a false sense of safety. A sealed contract that silently accepts
// new members is not sealed.
//
// Approach: parse every non-test .go file in this package directory, collect
// the receiver type names of every `isMessage()` and `isAgentTurnPayload()`
// method, and compare against the expected closed set. Adding a new
// implementor without declaring it in the expected set fails the test —
// because the scanner sees it in source even though no hand-written list
// mentions it.

// expectedMessageTypes is the closed set of types implementing Message.
// Add a type here WHEN you add a new Message implementation in message_bus.go.
var expectedMessageTypes = map[string]bool{
	"InboundMessage":     true,
	"OutboundMessage":    true,
	"AgentTurnStarted":   true,
	"AgentTurnCompleted": true,
	"AgentTurnFailed":    true,
	"AgentTurnCanceled":  true,
	"HumanRequested":     true,
	"HumanResponded":     true,
	"AssistantStatus":    true,
	"ToolCalled":         true,
	"ToolResult":         true,
}

// expectedPayloadTypes is the closed set of types implementing
// AgentTurnPayload.
var expectedPayloadTypes = map[string]bool{
	"FlowPayload":  true,
	"AgentPayload": true,
}

// packageDir returns the directory of this test file at runtime, so the
// source scan works regardless of where `go test` is invoked from.
func packageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate package source dir")
	}
	return filepath.Dir(file)
}

// scanSealedReceivers parses every non-test .go file in dir and returns the
// set of receiver type names that define a method named exactly methodName.
// A type appears once regardless of how many files mention it.
func scanSealedReceivers(t *testing.T, dir, methodName string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	found := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range parsed.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name == nil || fd.Name.Name != methodName {
				continue
			}
			if fd.Recv == nil || len(fd.Recv.List) == 0 {
				continue
			}
			// Receiver type may be plain (T) or pointer (*T). Unwrap.
			recvType := fd.Recv.List[0].Type
			if star, ok := recvType.(*ast.StarExpr); ok {
				recvType = star.X
			}
			if ident, ok := recvType.(*ast.Ident); ok {
				found[ident.Name] = true
			}
		}
	}
	return found
}

// TestMessageSealIsClosedAgainstSource scans the package source for every
// type implementing isMessage() and asserts the set equals expectedMessageTypes.
// A new Message type added to message_bus.go without updating
// expectedMessageTypes fails here.
func TestMessageSealIsClosedAgainstSource(t *testing.T) {
	actual := scanSealedReceivers(t, packageDir(t), "isMessage")
	assertClosedSet(t, "Message", expectedMessageTypes, actual)
}

// TestPayloadSealIsClosedAgainstSource does the same for AgentTurnPayload.
func TestPayloadSealIsClosedAgainstSource(t *testing.T) {
	actual := scanSealedReceivers(t, packageDir(t), "isAgentTurnPayload")
	assertClosedSet(t, "AgentTurnPayload", expectedPayloadTypes, actual)
}

// assertClosedSet reports three failure modes:
//   - a source implementor not declared expected (silent new member — the
//     W3 bug),
//   - an expected type with no source implementor (stale declaration),
//   - full agreement (pass).
func assertClosedSet(t *testing.T, ifaceName string, expected, actual map[string]bool) {
	t.Helper()
	var undeclared, stale []string
	for name := range actual {
		if !expected[name] {
			undeclared = append(undeclared, name)
		}
	}
	for name := range expected {
		if !actual[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(undeclared)
	sort.Strings(stale)
	if len(undeclared) > 0 {
		t.Errorf("NEW %s implementor(s) found in source but not declared expected: %v — "+
			"add them to expected%sTypes if intentional, or remove the isMessage/isAgentTurnPayload "+
			"method if stray (this is the PR #31 W3 sealed-leak guard)",
			ifaceName, undeclared, ifaceName)
	}
	if len(stale) > 0 {
		t.Errorf("expected %s type(s) have no source implementation: %v — "+
			"remove them from expected%sTypes", ifaceName, stale, ifaceName)
	}
}
