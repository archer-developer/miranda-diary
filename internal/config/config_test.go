package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_MissingConfigFileFailsClosed is the regression test for the bug
// class known_users exists to prevent: unlike every other field, KnownUsers
// has no baked-in default (see Default()), so a deployment with no
// config.yaml at all — or one that forgot known_users — must fail at
// startup instead of silently running a server that then rejects (or
// worse, silently mis-attributes) every tool call.
func TestLoad_MissingConfigFileFailsClosed(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "known_users")
}

func TestLoad_KnownUsersTrimmedAndValidated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("known_users: [\" archer \", \"anna\"]\n"), 0o644))

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"archer", "anna"}, cfg.KnownUsers)
}

func TestLoad_DuplicateKnownUsersRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("known_users: [\"archer\", \"archer\"]\n"), 0o644))

	_, err := Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}
