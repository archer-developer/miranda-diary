package mcpserver

import (
	"context"
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-diary/internal/config"
	"github.com/archer-developer/miranda-diary/internal/diary"
)

// fakeEmbedder returns a fixed-length zero vector regardless of input — the
// handlers under test here care about user_id validation, not embedding
// content, so a real Gemini call would only slow the suite down.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}

func newTestStore(t *testing.T) *diary.Store {
	t.Helper()
	s, err := diary.New(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var testUsers = []string{"archer", "anna"}

// TestMCPServer_UnknownUserRejected is the regression test for the bug this
// validation exists to catch: every tool call carries user_id as a plain
// model-supplied string (see config.Config.Users), so a caller passing
// a wrong-but-plausible value (a display name, a typo, another user's id
// picked up from context) must fail loudly instead of silently writing to
// or reading from an unintended bucket.
func TestMCPServer_UnknownUserRejected(t *testing.T) {
	store := newTestStore(t)
	logger := slog.New(slog.DiscardHandler)
	searchCfg := config.SearchConfig{DefaultLimit: 10, MaxLimit: 50}

	ctx := context.Background()
	req := &mcp.CallToolRequest{}

	t.Run("add_record", func(t *testing.T) {
		h := addRecordHandler(store, fakeEmbedder{}, testUsers, logger)
		_, _, err := h(ctx, req, AddRecordInput{UserID: "Саша", Content: "hello"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown user_id")
	})

	t.Run("search", func(t *testing.T) {
		h := searchHandler(store, fakeEmbedder{}, searchCfg, testUsers, logger)
		_, _, err := h(ctx, req, SearchInput{UserID: "sasha", Query: "hello"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown user_id")
	})

	t.Run("remove", func(t *testing.T) {
		h := removeHandler(store, testUsers, logger)
		_, _, err := h(ctx, req, RemoveInput{UserID: "alexander", ID: "some-id"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown user_id")
	})

	t.Run("empty user_id", func(t *testing.T) {
		h := addRecordHandler(store, fakeEmbedder{}, testUsers, logger)
		_, _, err := h(ctx, req, AddRecordInput{UserID: "  ", Content: "hello"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "user_id is required")
	})
}

// TestMCPServer_KnownUserAccepted confirms the allowlist doesn't break the
// legitimate case — a configured user's id must keep working end to end.
func TestMCPServer_KnownUserAccepted(t *testing.T) {
	store := newTestStore(t)
	logger := slog.New(slog.DiscardHandler)

	h := addRecordHandler(store, fakeEmbedder{}, testUsers, logger)
	_, out, err := h(context.Background(), &mcp.CallToolRequest{}, AddRecordInput{UserID: "archer", Content: "hello"})
	require.NoError(t, err)
	assert.NotEmpty(t, out.ID)
}

func TestResolveUser(t *testing.T) {
	users := []string{"anna", "archer"}

	got, err := resolveUser(users, "test", " archer ")
	require.NoError(t, err)
	assert.Equal(t, "archer", got)

	_, err = resolveUser(users, "test", "")
	require.Error(t, err)

	_, err = resolveUser(users, "test", "anya")
	require.Error(t, err)
}
