package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	defaultSearchMaxResults = 20
	maxSearchResults        = 50
	maxSearchFileBytes      = 1_000_000
)

type SearchFileTool struct{ fileTool }

func NewSearchFileTool(workspaceRoot string, readRoots, writeRoots []string) *SearchFileTool {
	return &SearchFileTool{newFileTool(workspaceRoot, readRoots, writeRoots)}
}

func (t *SearchFileTool) Name() string { return "search_file" }

func (t *SearchFileTool) Description() string {
	return "Search UTF-8 text files in the Xira workspace or configured sandbox roots and return matching paths, line numbers, and short snippets."
}

func (t *SearchFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":       map[string]any{"type": "string", "description": "Literal text to search for, case-insensitive."},
			"root":        map[string]any{"type": "string", "description": "Directory or file path to search, within the workspace or configured sandbox roots. Defaults to workspace root."},
			"max_results": map[string]any{"type": "integer", "description": "Maximum number of matches to return. Defaults to 20 and caps at 50."},
		},
		"required": []string{"query"},
	}
}

func (t *SearchFileTool) Execute(ctx context.Context, args map[string]any) (map[string]any, error) {
	query, _ := args["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	root, _ := args["root"].(string)
	if strings.TrimSpace(root) == "" {
		root = "."
	}
	rootPath, err := t.resolveSearchRoot(ctx, root)
	if err != nil {
		return nil, err
	}
	maxResults := defaultSearchMaxResults
	if raw, ok := numberArg(args, "max_results"); ok && raw > 0 {
		maxResults = raw
	}
	if maxResults > maxSearchResults {
		maxResults = maxSearchResults
	}

	var matches []map[string]any
	var totalMatches int
	var searchedFiles int
	var skippedFiles int
	addMatches := func(path string) error {
		if len(matches) >= maxResults {
			return nil
		}
		fileMatches, skipped, err := t.searchOneFile(path, query)
		if err != nil {
			return err
		}
		if skipped {
			skippedFiles++
			return nil
		}
		searchedFiles++
		totalMatches += len(fileMatches)
		remaining := maxResults - len(matches)
		if len(fileMatches) > remaining {
			fileMatches = fileMatches[:remaining]
		}
		matches = append(matches, fileMatches...)
		return nil
	}

	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		if err := addMatches(rootPath); err != nil {
			return nil, err
		}
		return t.searchOutput(rootPath, query, matches, totalMatches, searchedFiles, skippedFiles, maxResults), nil
	}

	err = filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if entry.IsDir() {
			if shouldSkipSearchDir(entry.Name()) && path != rootPath {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= maxResults {
			return filepath.SkipAll
		}
		if !looksSearchableTextFile(entry.Name()) {
			skippedFiles++
			return nil
		}
		return addMatches(path)
	})
	if err != nil && err != filepath.SkipAll {
		return nil, err
	}
	return t.searchOutput(rootPath, query, matches, totalMatches, searchedFiles, skippedFiles, maxResults), nil
}

func (t *SearchFileTool) searchOneFile(path, query string) ([]map[string]any, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, false, err
	}
	if info.IsDir() || info.Size() > maxSearchFileBytes {
		return nil, true, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	if !utf8.Valid(data) {
		return nil, true, nil
	}
	lowerQuery := strings.ToLower(query)
	lines := strings.Split(string(data), "\n")
	var matches []map[string]any
	for i, line := range lines {
		if !strings.Contains(strings.ToLower(line), lowerQuery) {
			continue
		}
		matches = append(matches, map[string]any{
			"path":    normalizeSearchPath(t.workspaceRoot, path),
			"line":    i + 1,
			"snippet": searchSnippet(line, query),
		})
	}
	return matches, false, nil
}

func (t *SearchFileTool) resolveSearchRoot(ctx context.Context, rawRoot string) (string, error) {
	// resolveReadPathCtx（#126 overlay）已经处理路径在可读 roots 内的约束
	// （workspace ∪ allow ∪ readonly + 私有层），无需额外的 workspace-only 检查。
	return t.resolveReadPathCtx(ctx, rawRoot)
}

func (t *SearchFileTool) searchOutput(rootPath, query string, matches []map[string]any, totalMatches, searchedFiles, skippedFiles, maxResults int) map[string]any {
	sort.Slice(matches, func(i, j int) bool {
		left := fmt.Sprintf("%s:%04d", matches[i]["path"], matches[i]["line"])
		right := fmt.Sprintf("%s:%04d", matches[j]["path"], matches[j]["line"])
		return left < right
	})
	return map[string]any{
		"root":           normalizeSearchPath(t.workspaceRoot, rootPath),
		"query":          query,
		"matches":        matches,
		"match_count":    len(matches),
		"total_matches":  totalMatches,
		"searched_files": searchedFiles,
		"skipped_files":  skippedFiles,
		"truncated":      totalMatches > len(matches) || len(matches) >= maxResults,
	}
}

func shouldSkipSearchDir(name string) bool {
	switch name {
	case ".git", ".xira", ".cache", "node_modules", "vendor":
		return true
	default:
		return false
	}
}

func looksSearchableTextFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".txt", ".yaml", ".yml", ".json", ".csv", ".go", ".py", ".js", ".ts", ".tsx", ".jsx":
		return true
	default:
		return false
	}
}

func normalizeSearchPath(workspaceRoot, path string) string {
	if rel, err := filepath.Rel(workspaceRoot, path); err == nil && !strings.HasPrefix(rel, "..") {
		path = rel
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func searchSnippet(line, query string) string {
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) <= 180 {
		return line
	}
	idx := runeIndex(strings.ToLower(line), strings.ToLower(query))
	if idx < 0 {
		return truncateText(line, 180)
	}
	runes := []rune(line)
	start := idx - 60
	if start < 0 {
		start = 0
	}
	end := idx + utf8.RuneCountInString(query) + 120
	if end > len(runes) {
		end = len(runes)
	}
	snippet := strings.TrimSpace(string(runes[start:end]))
	if start > 0 {
		snippet = "..." + snippet
	}
	if end < len(runes) {
		snippet += "..."
	}
	return snippet
}

func runeIndex(value, query string) int {
	if query == "" {
		return -1
	}
	values := []rune(value)
	queries := []rune(query)
	if len(queries) > len(values) {
		return -1
	}
	for i := 0; i <= len(values)-len(queries); i++ {
		match := true
		for j := range queries {
			if values[i+j] != queries[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func truncateText(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "..."
}
