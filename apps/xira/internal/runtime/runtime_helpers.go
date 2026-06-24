package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// runtime_helpers.go: small generic helpers shared across the runtime package.
// Relocated here from delegation.go when delegate_agent was retired (Phase 6a,
// #55) so the kept tools (human.request, status, spawn_turn, poll_turn) and
// their tests still compile.

// stringArg extracts a trimmed string field from an ADK tool args map.
func stringArg(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	value, ok := args[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// stringSliceFromAny coerces a tool-arg value ([]string, []any, string) into
// a sorted, deduped-free []string.
func stringSliceFromAny(value any) []string {
	switch v := value.(type) {
	case []string:
		out := append([]string(nil), v...)
		sort.Strings(out)
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" {
				out = append(out, text)
			}
		}
		sort.Strings(out)
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{strings.TrimSpace(v)}
	default:
		return nil
	}
}

// writeJSONFile writes value as indented JSON to path, creating parent dirs.
func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
