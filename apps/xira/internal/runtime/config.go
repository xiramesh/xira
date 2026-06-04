package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ai-daming/xira/internal/agents"
	"github.com/ai-daming/xira/internal/entrypoints"
	"github.com/ai-daming/xira/internal/routing"
)

const defaultConfigPath = "xira.yaml"

type runtimeConfigFile struct {
	Workspace      string       `yaml:"workspace"`
	DefaultAgentID string       `yaml:"default_agent"`
	RunRoot        string       `yaml:"run_root"`
	SessionRoot    string       `yaml:"session_root"`
	StateRoot      string       `yaml:"state_root"`
	Entrypoints    string       `yaml:"entrypoints"`
	Pricing        UsagePricing `yaml:"pricing"`
}

type entrypointsConfigFile struct {
	Entrypoints []entrypoints.Definition `yaml:"entrypoints"`
}

type resolvedRuntimeConfig struct {
	ConfigPath        string
	ConfigLoaded      bool
	WorkspaceExplicit bool
	WorkspaceRoot     string
	DefaultAgentID    string
	RunRoot           string
	SessionRoot       string
	StateRoot         string
	Pricing           UsagePricing
	Entrypoints       []entrypoints.Definition
}

func resolveRuntimeConfig(cfg Config) (resolvedRuntimeConfig, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return resolvedRuntimeConfig{}, err
	}

	configInput := strings.TrimSpace(cfg.ConfigPath)
	if configInput == "" {
		configInput = defaultConfigPath
	}
	configPath := resolveRelativePath(cwd, configInput)
	configFile, configLoaded, err := readRuntimeConfigFile(configPath, isDefaultConfigInput(cfg.ConfigPath))
	if err != nil {
		return resolvedRuntimeConfig{}, err
	}

	baseDir := cwd
	if configLoaded {
		baseDir = filepath.Dir(configPath)
	}

	workspaceExplicit := strings.TrimSpace(cfg.WorkspaceRoot) != ""
	workspace := strings.TrimSpace(cfg.WorkspaceRoot)
	if workspace == "" {
		workspace = strings.TrimSpace(configFile.Workspace)
	}
	if workspace == "" {
		if configLoaded {
			workspace = "workspace"
		} else {
			workspace = cwd
		}
	}
	workspace = resolveRelativePath(baseDir, workspace)

	stateRoot := strings.TrimSpace(cfg.StateRoot)
	if stateRoot == "" {
		stateRoot = strings.TrimSpace(configFile.StateRoot)
	}

	runRoot := strings.TrimSpace(cfg.RunRoot)
	if runRoot == "" {
		runRoot = strings.TrimSpace(configFile.RunRoot)
	}
	if runRoot == "" && stateRoot != "" {
		runRoot = filepath.Join(stateRoot, "runs")
	}
	if runRoot == "" {
		runRoot = ".xira/runs"
	}
	runRoot = resolveRelativePath(baseDir, runRoot)

	sessionRoot := strings.TrimSpace(cfg.SessionRoot)
	if sessionRoot == "" {
		sessionRoot = strings.TrimSpace(configFile.SessionRoot)
	}
	if sessionRoot == "" && stateRoot != "" {
		sessionRoot = filepath.Join(stateRoot, "sessions")
	}
	if sessionRoot == "" {
		sessionRoot = filepath.Join(filepath.Dir(runRoot), "sessions")
	}
	sessionRoot = resolveRelativePath(baseDir, sessionRoot)

	if stateRoot == "" {
		stateRoot = filepath.Join(filepath.Dir(runRoot), "state")
	}
	stateRoot = resolveRelativePath(baseDir, stateRoot)

	entrypointsPath := strings.TrimSpace(configFile.Entrypoints)
	entrypointsRequired := false
	if entrypointsPath != "" {
		entrypointsPath = resolveRelativePath(baseDir, entrypointsPath)
		entrypointsRequired = true
	} else if configLoaded {
		entrypointsPath = filepath.Join(workspace, "entrypoints.yaml")
	}
	entrypointDefs, err := readEntrypointsFile(entrypointsPath, entrypointsRequired)
	if err != nil {
		return resolvedRuntimeConfig{}, err
	}

	defaultAgentID := strings.TrimSpace(cfg.DefaultAgentID)
	if defaultAgentID == "" {
		defaultAgentID = strings.TrimSpace(configFile.DefaultAgentID)
	}
	if defaultAgentID == "" {
		defaultAgentID = agents.DefaultAgentID
	}

	return resolvedRuntimeConfig{
		ConfigPath:        configPath,
		ConfigLoaded:      configLoaded,
		WorkspaceExplicit: workspaceExplicit,
		WorkspaceRoot:     workspace,
		DefaultAgentID:    defaultAgentID,
		RunRoot:           runRoot,
		SessionRoot:       sessionRoot,
		StateRoot:         stateRoot,
		Pricing:           normalizeUsagePricing(configFile.Pricing),
		Entrypoints:       entrypointDefs,
	}, nil
}

func readRuntimeConfigFile(path string, optional bool) (runtimeConfigFile, bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return runtimeConfigFile{}, false, nil
		}
		return runtimeConfigFile{}, false, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg runtimeConfigFile
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return runtimeConfigFile{}, false, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, true, nil
}

func readEntrypointsFile(path string, required bool) ([]entrypoints.Definition, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read entrypoints %s: %w", path, err)
	}
	var cfg entrypointsConfigFile
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return nil, fmt.Errorf("parse entrypoints %s: %w", path, err)
	}
	return cfg.Entrypoints, nil
}

func loadAgentManager(resolved resolvedRuntimeConfig) (*agents.Manager, string, error) {
	if resolved.ConfigLoaded || resolved.WorkspaceExplicit {
		manager, err := agents.LoadFromWorkspace(resolved.WorkspaceRoot)
		if err != nil {
			return nil, "", err
		}
		return manager, "workspace", nil
	}
	manager, err := agents.NewBuiltinManager()
	if err != nil {
		return nil, "", err
	}
	return manager, "builtin", nil
}

func sessionPolicyForProfile(profile agents.Profile, fallback routing.SessionPolicy) routing.SessionPolicy {
	if len(profile.Session.Dimensions) == 0 && len(profile.Session.IdentityLinks) == 0 {
		return routing.NormalizeSessionPolicy(fallback)
	}
	return routing.NormalizeSessionPolicy(routing.SessionPolicy{
		Dimensions:    profile.Session.Dimensions,
		IdentityLinks: profile.Session.IdentityLinks,
	})
}

func resolveRelativePath(baseDir, path string) string {
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	abs, err := filepath.Abs(filepath.Join(baseDir, path))
	if err != nil {
		return filepath.Clean(filepath.Join(baseDir, path))
	}
	return abs
}

func isDefaultConfigInput(path string) bool {
	path = strings.TrimSpace(path)
	return path == "" || filepath.Clean(path) == defaultConfigPath
}
