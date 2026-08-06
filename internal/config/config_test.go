package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_MissingConfigFileFailsClosed is the regression test for the bug
// class the users field exists to prevent: unlike every other field, Users
// has no baked-in default (see Default()), so a deployment with no
// config.yaml at all — or one that forgot users — must fail at startup
// instead of silently running a server that then rejects (or worse, silently
// mis-attributes) every tool call.
func TestLoad_MissingConfigFileFailsClosed(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "users")
}

func TestLoad_UsersTrimmedAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("users:\n  - id: \" archer \"\n  - id: \"anna\"\n"), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, []UserConfig{{ID: "archer"}, {ID: "anna"}}, cfg.Users)
}

func TestLoad_DuplicateUsersRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("users:\n  - id: \"archer\"\n  - id: \"archer\"\n"), 0o644))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

// TestLoad_MultipleFilesMergeInOrder is the regression test for the
// multi-file merge behavior config.Load gained when it switched from a
// single config.yaml path to a variadic list of config/*.yaml files (see
// cmd/miranda-diary/main.go's configFilePaths): later files must override
// earlier ones field-by-field, not wholesale.
func TestLoad_MultipleFilesMergeInOrder(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "a-base.yaml")
	override := filepath.Join(dir, "b-override.yaml")
	require.NoError(t, os.WriteFile(base, []byte("http_addr: \":9999\"\nusers:\n  - id: \"archer\"\n"), 0o644))
	require.NoError(t, os.WriteFile(override, []byte("logging:\n  level: \"debug\"\n"), 0o644))

	cfg, err := Load(base, override)
	require.NoError(t, err)
	assert.Equal(t, ":9999", cfg.HTTPAddr)
	assert.Equal(t, []UserConfig{{ID: "archer"}}, cfg.Users)
	assert.Equal(t, "debug", cfg.Logging.Level)
}
