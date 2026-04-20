package workspace

import (
	"encoding/json"
	"fmt"
	"os"
)

const configVersion = 1

// config is the on-disk schema for .agent-infra/config.json.
// Fields are JSON-tagged for snake_case on disk; version gates schema migrations.
type config struct {
	Version     int    `json:"version"`
	AgentID     string `json:"agent_id"`
	NATSURL     string `json:"nats_url"`
	LeafHTTPURL string `json:"leaf_http_url"`
	RepoPath    string `json:"repo_path"`
	CreatedAt   string `json:"created_at"`
}

func saveConfig(path string, c config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

func loadConfig(path string) (config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read config: %w", err)
	}
	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		return config{}, fmt.Errorf("parse config: %w", err)
	}
	if c.Version != configVersion {
		return config{}, fmt.Errorf(
			"unsupported config version %d (expected %d)", c.Version, configVersion)
	}
	return c, nil
}
