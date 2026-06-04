package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ai-daming/xira/internal/agents"
	"github.com/ai-daming/xira/internal/model/deepseek"
)

const groundingVerificationCheck = "grounding_required_reads"

type groundingPreparation struct {
	result    GroundingResult
	context   string
	toolCalls []ToolCallRecord
}

func (s *Service) prepareGrounding(
	ctx context.Context,
	profile agents.Profile,
	message string,
	recordEvent func(kind, source, message string, payload map[string]any),
	recordAudit func(action, target string, allowed bool, reason string, meta map[string]any),
) groundingPreparation {
	policy := profile.Knowledge
	if !knowledgeConfigured(policy) {
		return groundingPreparation{}
	}
	required, matchedRules := requiredKnowledgeFiles(policy, profile.ID, message)
	result := GroundingResult{
		Status:        "passed",
		Root:          cleanKnowledgeRoot(policy.Root, profile.ID),
		MatchedRules:  matchedRules,
		RequiredFiles: required,
	}
	if len(required) == 0 {
		result.Status = "not_required"
		return groundingPreparation{result: result}
	}

	var contextParts []string
	var toolCalls []ToolCallRecord
	for _, path := range required {
		args, _ := json.Marshal(map[string]any{"path": path, "grounding_required": true})
		rec := s.executeToolCall(ctx, profile, deepseek.ToolCall{
			Type: "function",
			Function: deepseek.ToolCallFunction{
				Name:      "read_file",
				Arguments: string(args),
			},
		}, recordEvent, recordAudit)
		toolCalls = append(toolCalls, rec)
		result.ReadFiles = appendSuccessfulReadFile(result.ReadFiles, s.workspace, rec)
		if rec.Error != "" {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %s", path, rec.Error))
		} else if content, ok := rec.Output["content"].(string); ok {
			contextParts = append(contextParts, formatGroundingContextFile(path, content))
		}
		result.ContextChars += outputContentChars(rec.Output)
	}
	result.ReadFiles = uniqueSortedStrings(result.ReadFiles)
	result.Errors = uniqueSortedStrings(result.Errors)
	result.MissingFiles = missingRequiredFiles(required, result.ReadFiles)
	if len(result.Errors) > 0 || len(result.MissingFiles) > 0 {
		result.Status = "failed"
	}
	contextText := strings.Join(contextParts, "\n\n")
	if contextText != "" {
		contextText = "# Runtime Required Knowledge Context\n\nThese files were preloaded by Xira runtime because the grounding policy requires them for this turn. Use them as authoritative local knowledge for this answer.\n\n" + contextText
	}
	return groundingPreparation{result: result, context: contextText, toolCalls: toolCalls}
}

func (s *Service) finalizeGrounding(profile agents.Profile, userMessage, finalResponse string, toolCalls []ToolCallRecord, prepared GroundingResult) GroundingResult {
	policy := profile.Knowledge
	if !knowledgeConfigured(policy) {
		return GroundingResult{}
	}
	required, matchedRules := requiredKnowledgeFiles(policy, profile.ID, userMessage+"\n"+finalResponse)
	readFiles := successfulReadFilesFromToolCalls(s.workspace, toolCalls)
	result := prepared
	result.Root = cleanKnowledgeRoot(policy.Root, profile.ID)
	result.MatchedRules = uniqueSortedStrings(append(result.MatchedRules, matchedRules...))
	result.RequiredFiles = uniqueSortedStrings(append(result.RequiredFiles, required...))
	result.ReadFiles = uniqueSortedStrings(append(result.ReadFiles, readFiles...))
	result.MissingFiles = missingRequiredFiles(result.RequiredFiles, result.ReadFiles)
	result.Errors = uniqueSortedStrings(result.Errors)
	if len(result.RequiredFiles) == 0 {
		result.Status = "not_required"
		return result
	}
	if len(result.Errors) > 0 || len(result.MissingFiles) > 0 {
		result.Status = "failed"
		return result
	}
	result.Status = "passed"
	return result
}

func mergeGroundingVerification(result VerificationResult, grounding GroundingResult) VerificationResult {
	if grounding.Status == "" || grounding.Status == "not_required" {
		return result
	}
	result.Checks = append(result.Checks, groundingVerificationCheck)
	if grounding.Status != "passed" {
		result.Status = "failed"
		for _, path := range grounding.MissingFiles {
			result.Errors = append(result.Errors, "grounding missing required file: "+path)
		}
		for _, err := range grounding.Errors {
			result.Errors = append(result.Errors, "grounding read failed: "+err)
		}
	}
	result.Checks = uniqueSortedStrings(result.Checks)
	result.Errors = uniqueSortedStrings(result.Errors)
	return result
}

func knowledgeConfigured(policy agents.KnowledgePolicy) bool {
	return strings.TrimSpace(policy.Root) != "" || len(policy.Default) > 0 || len(policy.Rules) > 0
}

func requiredKnowledgeFiles(policy agents.KnowledgePolicy, agentID, text string) ([]string, []string) {
	root := cleanKnowledgeRoot(policy.Root, agentID)
	var files []string
	for _, path := range policy.Default {
		files = append(files, knowledgePath(root, path))
	}
	var matched []string
	for _, rule := range policy.Rules {
		if !knowledgeRuleMatches(rule, text) {
			continue
		}
		matched = append(matched, knowledgeRuleID(rule))
		for _, path := range rule.Required {
			files = append(files, knowledgePath(root, path))
		}
	}
	return uniqueSortedStrings(files), uniqueSortedStrings(matched)
}

func knowledgeRuleMatches(rule agents.KnowledgeRule, text string) bool {
	text = strings.ToLower(text)
	for _, keyword := range rule.Keywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword != "" && strings.Contains(text, keyword) {
			return true
		}
	}
	if len(rule.Keywords) == 0 {
		when := strings.ToLower(strings.TrimSpace(rule.When))
		return when != "" && strings.Contains(text, when)
	}
	return false
}

func knowledgeRuleID(rule agents.KnowledgeRule) string {
	if strings.TrimSpace(rule.ID) != "" {
		return strings.TrimSpace(rule.ID)
	}
	return strings.TrimSpace(rule.When)
}

func cleanKnowledgeRoot(root, agentID string) string {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" && strings.TrimSpace(agentID) != "" {
		root = "kb/" + strings.Trim(strings.TrimSpace(agentID), "/")
	}
	return filepath.ToSlash(filepath.Clean(root))
}

func knowledgePath(root, path string) string {
	root = cleanKnowledgeRoot(root, "")
	path = strings.Trim(strings.TrimSpace(path), "/")
	if path == "" {
		return root
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if root == "" || strings.HasPrefix(path, root+"/") || path == root {
		return path
	}
	return filepath.ToSlash(filepath.Join(root, path))
}

func formatGroundingContextFile(path, content string) string {
	return "## " + path + "\n\n" + truncateRunes(strings.TrimSpace(content), 12000)
}

func outputContentChars(output map[string]any) int {
	if content, ok := output["content"].(string); ok {
		return utf8.RuneCountInString(content)
	}
	return 0
}

func appendSuccessfulReadFile(existing []string, workspace string, rec ToolCallRecord) []string {
	if rec.Name != "read_file" || rec.Error != "" {
		return existing
	}
	if path := readFilePathFromRecord(workspace, rec); path != "" {
		return append(existing, path)
	}
	return existing
}

func successfulReadFilesFromToolCalls(workspace string, calls []ToolCallRecord) []string {
	var out []string
	for _, rec := range calls {
		out = appendSuccessfulReadFile(out, workspace, rec)
	}
	return uniqueSortedStrings(out)
}

func readFilePathFromRecord(workspace string, rec ToolCallRecord) string {
	if path, ok := rec.Output["path"].(string); ok && strings.TrimSpace(path) != "" {
		return normalizeWorkspacePath(workspace, path)
	}
	if path, ok := rec.Input["path"].(string); ok && strings.TrimSpace(path) != "" {
		return normalizeWorkspacePath(workspace, path)
	}
	return ""
}

func normalizeWorkspacePath(workspace, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) && strings.TrimSpace(workspace) != "" {
		if rel, err := filepath.Rel(workspace, path); err == nil && !strings.HasPrefix(rel, "..") {
			path = rel
		}
	}
	path = filepath.ToSlash(filepath.Clean(path))
	path = strings.TrimPrefix(path, "./")
	return path
}

func missingRequiredFiles(required, read []string) []string {
	readSet := map[string]bool{}
	for _, path := range read {
		readSet[path] = true
	}
	var missing []string
	for _, path := range required {
		if !readSet[path] {
			missing = append(missing, path)
		}
	}
	return uniqueSortedStrings(missing)
}

func uniqueSortedStrings(values []string) []string {
	return sortStrings(uniqueStrings(values))
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func sortStrings(values []string) []string {
	sort.Strings(values)
	return values
}

func truncateRunes(value string, max int) string {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max]) + "\n...[truncated]"
}
