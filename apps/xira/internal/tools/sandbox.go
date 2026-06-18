package tools

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// expandHome expands a leading ~ (POSIX ~/ or a bare ~, plus the OS separator
// variant) to the user's home directory. Other ~-prefixed values are returned
// unchanged so the caller can reject them as non-absolute.
func expandHome(path string) (string, error) {
	if path == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// expandRoot resolves a single configured root into an absolute path. Roots
// must be absolute or start with ~; relative roots are rejected so access
// scope never depends on the working directory the runtime happens to run in.
func expandRoot(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	expanded, err := expandHome(raw)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(expanded) {
		return "", errors.New("root must be absolute or start with ~: " + raw)
	}
	abs, err := filepath.Abs(filepath.Clean(expanded))
	if err != nil {
		return "", err
	}
	return abs, nil
}

// ExpandRoots expands, dedupes, and sorts a list of configured roots. Empty
// entries are skipped. An invalid (relative) root is a hard error: a
// misconfigured root must fail loudly rather than silently widen or narrow
// the sandbox.
func ExpandRoots(raw []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		abs, err := expandRoot(item)
		if err != nil {
			return nil, err
		}
		if abs == "" {
			continue
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	sort.Strings(out)
	return out, nil
}

// mergeRoots concatenates root groups, cleans and dedupes them, preserving
// first-seen order (workspace first so relative-path resolution stays stable).
func mergeRoots(groups ...[]string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, group := range groups {
		for _, root := range group {
			root = filepath.Clean(root)
			if root == "" {
				continue
			}
			if _, ok := seen[root]; ok {
				continue
			}
			seen[root] = struct{}{}
			out = append(out, root)
		}
	}
	return out
}

// pathWithinRoots reports whether a cleaned absolute path is equal to or
// nested under any of the given roots. A root must already be absolute.
func pathWithinRoots(absPath string, roots []string) bool {
	absPath = filepath.Clean(absPath)
	for _, root := range roots {
		root = filepath.Clean(root)
		if absPath == root {
			return true
		}
		rel, err := filepath.Rel(root, absPath)
		if err != nil {
			continue
		}
		if rel == "." {
			return true
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		return true
	}
	return false
}
