// Package config loads miranda-diary's YAML configuration and merges it over
// built-in defaults, so the service runs with sane behavior even with an
// empty or partial config.yaml. The pattern mirrors miranda-code-execution-sandbox:
// Default() populates every field, Load() applies yaml.Unmarshal on top for
// a cheap partial merge, validate() rejects values that Default() would never
// produce but a hand-edited file could.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the root of the service's configuration tree.
type Config struct {
	HTTPAddr     string          `yaml:"http_addr"`
	AuthTokenEnv string          `yaml:"auth_token_env"`
	Database     DatabaseConfig  `yaml:"database"`
	Embedding    EmbeddingConfig `yaml:"embedding"`
	Search       SearchConfig    `yaml:"search"`
	Logging      LoggingConfig   `yaml:"logging"`
}

// DatabaseConfig controls the SQLite database file location.
type DatabaseConfig struct {
	// Path is the SQLite file path, relative to the process working directory.
	Path string `yaml:"path"`
}

// EmbeddingConfig configures the Gemini embedding model used to vectorize
// diary records and search queries.
type EmbeddingConfig struct {
	// APIKeyEnv is the name of the environment variable holding the Google AI
	// (Gemini) API key. The key itself is never read here — only the env var
	// name is stored in config so it can be audited without touching secrets.
	APIKeyEnv string `yaml:"api_key_env"`
	// Model is the Gemini embedding model identifier. text-embedding-004 is
	// the current recommended free-tier model: 768 dimensions, very good
	// multilingual quality, generous rate limits (1 500 req/day free).
	Model string `yaml:"model"`
}

// SearchConfig controls diary_search behavior.
type SearchConfig struct {
	// DefaultLimit is the number of results returned when the caller does not
	// specify a limit.
	DefaultLimit int `yaml:"default_limit"`
	// MaxLimit caps the limit a caller may request.
	MaxLimit int `yaml:"max_limit"`
}

// LoggingConfig controls slog output level.
type LoggingConfig struct {
	Level string `yaml:"level"`
}

// Default returns the built-in configuration. Every field has a safe, runnable
// value so a missing or empty config.yaml still produces a working service.
func Default() Config {
	return Config{
		HTTPAddr:     ":8789",
		AuthTokenEnv: "DIARY_MCP_TOKEN",
		Database: DatabaseConfig{
			Path: "data/diary.db",
		},
		Embedding: EmbeddingConfig{
			APIKeyEnv: "GEMINI_API_KEY",
			Model:     "text-embedding-004",
		},
		Search: SearchConfig{
			DefaultLimit: 10,
			MaxLimit:     50,
		},
		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// Load reads the YAML file at path and merges it over Default(). A missing
// file is not an error — defaults are used as-is.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("config: read %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func (c Config) validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: http_addr must not be empty")
	}
	if c.AuthTokenEnv == "" {
		return fmt.Errorf("config: auth_token_env must not be empty")
	}
	if c.Database.Path == "" {
		return fmt.Errorf("config: database.path must not be empty")
	}
	if c.Embedding.APIKeyEnv == "" {
		return fmt.Errorf("config: embedding.api_key_env must not be empty")
	}
	if c.Embedding.Model == "" {
		return fmt.Errorf("config: embedding.model must not be empty")
	}
	if c.Search.DefaultLimit < 1 {
		return fmt.Errorf("config: search.default_limit must be at least 1")
	}
	if c.Search.MaxLimit < c.Search.DefaultLimit {
		return fmt.Errorf("config: search.max_limit must be >= default_limit")
	}
	return nil
}
