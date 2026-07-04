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
//
// Symlink safety (#110 follow-up): both the path and the roots are resolved
// through EvalSymlinks (on the longest existing prefix, so writing a new file
// under an existing directory still works) before comparison. Without this,
// a symlink inside a root pointing outside (e.g. workspace/evil -> /etc)
// would let an agent write outside the boundary by traversing the symlink.
func pathWithinRoots(absPath string, roots []string) bool {
	resolvedPath := resolveSymlinkSafe(absPath)
	for _, root := range roots {
		resolvedRoot := resolveSymlinkSafe(filepath.Clean(root))
		if resolvedPath == resolvedRoot {
			return true
		}
		rel, err := filepath.Rel(resolvedRoot, resolvedPath)
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

// resolveSymlinkSafe resolves symlinks in a path, tolerating non-existent
// final segments (write_file creates new files whose path doesn't exist yet).
// It resolves the longest existing prefix and rejoins the non-existent tail,
// so an in-bound write to a brand-new file is still recognized as in-bound
// while a symlink earlier in the path is still resolved.
func resolveSymlinkSafe(p string) string {
	p = filepath.Clean(p)
	// Fast path: whole path exists, resolve it directly.
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	// Walk up until an existing ancestor, resolve that, then re-append the
	// non-existent tail. This mirrors how realpath -m handles missing segments.
	dir := p
	tail := ""
	for dir != "" && dir != "/" && dir != "." {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			if tail == "" {
				return resolved
			}
			return filepath.Join(resolved, tail)
		}
		tail = filepath.Join(filepath.Base(dir), tail)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Nothing in the path exists; return cleaned form (no symlink to resolve).
	return p
}
