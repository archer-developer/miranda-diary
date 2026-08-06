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
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the root of the service's configuration tree.
type Config struct {
	HTTPAddr     string          `yaml:"http_addr"`
	AuthTokenEnv string          `yaml:"auth_token_env"`
	Database     DatabaseConfig  `yaml:"database"`
	Embedding    EmbeddingConfig `yaml:"embedding"`
	Search       SearchConfig    `yaml:"search"`
	// KnownUsers is the closed set of user_id values every tool call is
	// checked against. This diary has no per-user credentials the way
	// miranda-yazio does, so nothing else would ever catch a caller (i.e.
	// Miranda's LLM) passing the wrong household member's id — with no
	// validation, a typo or a wrong-but-plausible id doesn't error, it just
	// silently opens a new, unsearchable user_id bucket. See
	// mcpserver.resolveUser.
	KnownUsers []string      `yaml:"known_users"`
	Logging    LoggingConfig `yaml:"logging"`
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
	// Model is the Gemini embedding model identifier. gemini-embedding-2 is
	// the current recommended free-tier model: very good
	// multilingual quality.
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
			Model:     "gemini-embedding-2",
		},
		Search: SearchConfig{
			DefaultLimit: 10,
			MaxLimit:     50,
		},
		// KnownUsers has no default value — who's in the household isn't
		// something to bake into source code, unlike every other field
		// here. config.yaml must set known_users explicitly; see validate().
		KnownUsers: nil,
		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// Load reads the YAML file at path and merges it over Default(). A missing
// file is not a read error — defaults are used as-is — but validate() still
// runs either way: unlike every other field, KnownUsers has no built-in
// default (see Default()), so a genuinely missing/empty config.yaml must
// fail fast at startup here, not silently start a server that then rejects
// every tool call at request time.
func Load(path string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}
	} else if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}

	// A hand-edited YAML scalar can carry stray whitespace; every tool
	// call's user_id is trimmed before the allowlist check (see
	// mcpserver.resolveUser), so an untrimmed entry here would make that
	// user permanently unreachable — normalize before validate() so the
	// trimmed form is also what duplicate-detection compares.
	for i := range cfg.KnownUsers {
		cfg.KnownUsers[i] = strings.TrimSpace(cfg.KnownUsers[i])
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
	if len(c.KnownUsers) == 0 {
		return fmt.Errorf("config: known_users must not be empty — list every household member's user_id explicitly in config.yaml")
	}
	seen := make(map[string]bool, len(c.KnownUsers))
	for i, u := range c.KnownUsers {
		if u == "" {
			return fmt.Errorf("config: known_users[%d] must not be empty", i)
		}
		if seen[u] {
			return fmt.Errorf("config: known_users[%d] %q is a duplicate — entries must be unique", i, u)
		}
		seen[u] = true
	}
	return nil
}
