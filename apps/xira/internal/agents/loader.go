package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/xiramesh/xira/internal/fileutil"
	"github.com/xiramesh/xira/internal/tools"
)

const (
	profileFileName = "PROFILE.md"
	soulFileName    = "SOUL.md"
)

type profileFrontmatter struct {
	ID           string             `yaml:"id"`
	Name         string             `yaml:"name"`
	Version      string             `yaml:"version"`
	Description  string             `yaml:"description"`
	ModelPolicy  ModelPolicy        `yaml:"model_policy"`
	Context      ContextPolicy      `yaml:"context"`
	Skills       []string           `yaml:"skills"`
	MCPServers   []string           `yaml:"mcp_servers"`
	Tools         []string           `yaml:"tools"`
	AllowRoots    []string           `yaml:"allow_roots,omitempty"`
	ReadonlyRoots []string           `yaml:"readonly_roots,omitempty"`
	Session       SessionPolicy      `yaml:"session"`
	Permissions  Permissions        `yaml:"permissions"`
	Delegation   DelegationPolicy   `yaml:"delegation"`
	Verification VerificationPolicy `yaml:"verification"`
	Artifacts    ArtifactPolicy     `yaml:"artifacts"`
	Evolution    EvolutionPolicy    `yaml:"evolution"`
}

func LoadFromWorkspace(workspaceRoot string) (*Manager, error) {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot == "" {
		return nil, fmt.Errorf("workspace root is required")
	}
	agentsRoot := filepath.Join(workspaceRoot, "agents")
	entries, err := os.ReadDir(agentsRoot)
	if err != nil {
		return nil, fmt.Errorf("read agents directory %s: %w", agentsRoot, err)
	}

	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(agentsRoot, entry.Name()))
		}
	}
	sort.Strings(dirs)

	profiles := make([]Profile, 0, len(dirs))
	for _, dir := range dirs {
		_, err := fileutil.FindFileEqualFold(dir, profileFileName)
		if err != nil {
			if fileutil.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		profile, err := LoadProfileDir(dir)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no agent profiles found under %s", agentsRoot)
	}
	return NewManager(profiles)
}

func LoadProfileDir(agentDir string) (Profile, error) {
	profilePath, err := fileutil.FindFileEqualFold(agentDir, profileFileName)
	if err != nil {
		return Profile{}, err
	}
	content, err := os.ReadFile(profilePath)
	if err != nil {
		return Profile{}, fmt.Errorf("read %s: %w", profilePath, err)
	}

	frontmatter, body, err := parseProfileMarkdown(string(content))
	if err != nil {
		return Profile{}, fmt.Errorf("parse %s: %w", profilePath, err)
	}

	dirID := filepath.Base(agentDir)
	if frontmatter.ID != dirID {
		return Profile{}, fmt.Errorf("%s id %q must match agent directory %q", profileFileName, frontmatter.ID, dirID)
	}

	instructions := make([]string, 0, 2)
	if trimmed := strings.TrimSpace(body); trimmed != "" {
		instructions = append(instructions, trimmed)
	}
	soulPath, err := fileutil.FindFileEqualFold(agentDir, soulFileName)
	if err != nil {
		return Profile{}, err
	}
	soul, err := os.ReadFile(soulPath)
	if err != nil {
		return Profile{}, fmt.Errorf("read %s: %w", soulPath, err)
	}
	if trimmed := strings.TrimSpace(string(soul)); trimmed != "" {
		instructions = append(instructions, trimmed)
	} else {
		return Profile{}, fmt.Errorf("%s is required", soulPath)
	}

	permissions := frontmatter.Permissions
	if len(frontmatter.Tools) > 0 {
		permissions.Tools = frontmatter.Tools
	}
	if len(frontmatter.AllowRoots) > 0 {
		permissions.AllowRoots = frontmatter.AllowRoots
	}
	if len(frontmatter.ReadonlyRoots) > 0 {
		permissions.ReadonlyRoots = frontmatter.ReadonlyRoots
	}

	profile := Profile{
		ID:           frontmatter.ID,
		Name:         frontmatter.Name,
		Version:      frontmatter.Version,
		Description:  frontmatter.Description,
		ModelPolicy:  frontmatter.ModelPolicy,
		Instructions: instructions,
		Context:      frontmatter.Context,
		Skills:       frontmatter.Skills,
		MCPServers:   frontmatter.MCPServers,
		Session:      frontmatter.Session,
		Permissions:  permissions,
		Delegation:   frontmatter.Delegation,
		Verification: frontmatter.Verification,
		Artifacts:    frontmatter.Artifacts,
		Evolution:    frontmatter.Evolution,
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, fmt.Errorf("invalid profile %q: %w", profile.ID, err)
	}
	allowRoots, err := tools.ExpandRoots(profile.Permissions.AllowRoots)
	if err != nil {
		return Profile{}, fmt.Errorf("invalid profile %q allow_roots: %w", profile.ID, err)
	}
	readonlyRoots, err := tools.ExpandRoots(profile.Permissions.ReadonlyRoots)
	if err != nil {
		return Profile{}, fmt.Errorf("invalid profile %q readonly_roots: %w", profile.ID, err)
	}
	profile.Permissions.AllowRoots = allowRoots
	profile.Permissions.ReadonlyRoots = readonlyRoots
	return profile, nil
}

func parseProfileMarkdown(content string) (profileFrontmatter, string, error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return profileFrontmatter{}, "", fmt.Errorf("YAML frontmatter is required")
	}
	rest := normalized[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return profileFrontmatter{}, "", fmt.Errorf("YAML frontmatter closing delimiter is required")
	}

	var frontmatter profileFrontmatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &frontmatter); err != nil {
		return profileFrontmatter{}, "", fmt.Errorf("YAML frontmatter is invalid: %w", err)
	}

	body := rest[end+len("\n---"):]
	if strings.HasPrefix(body, "\n") {
		body = body[1:]
	}
	return frontmatter, body, nil
}
