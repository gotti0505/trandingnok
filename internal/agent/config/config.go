// Package config holds the AgentConfig that drives the LocalAgent process.
// The config file (config.agent.yaml) must NEVER be committed to version control
// because it contains the Exchange API credentials.
package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// AgentConfig is the complete configuration for one LocalAgent instance.
type AgentConfig struct {
	SaaSURL  string         `yaml:"saas_url"`  // e.g. "https://your-saas.example.com"
	Email    string         `yaml:"email"`
	Password string         `yaml:"password"`
	Exchange ExchangeConfig `yaml:"exchange"`
}

// ExchangeConfig holds the exchange credentials.
// These values must ONLY exist in the local config file, never in SaaS or DB.
type ExchangeConfig struct {
	Name       string `yaml:"name"`       // e.g. "bitget"
	APIKey     string `yaml:"api_key"`
	SecretKey  string `yaml:"secret_key"`
	Passphrase string `yaml:"passphrase"`
	Sandbox    bool   `yaml:"sandbox"`
}

// Load reads the agent config from path.
func Load(path string) (*AgentConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AgentConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
