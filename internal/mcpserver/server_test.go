package mcpserver

import (
	"context"
	"log/slog"
	"testing"
	"time"

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

var testUserMap = map[string]config.UserConfig{
	"archer": {ID: "archer"},
	"anna":   {ID: "anna"},
}

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
		h := addRecordHandler(store, fakeEmbedder{}, testUsers, testUserMap, nil, logger)
		_, _, err := h(ctx, req, AddRecordInput{UserID: "Саша", Content: "hello"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown user_id")
	})

	t.Run("search", func(t *testing.T) {
		h := searchHandler(store, fakeEmbedder{}, searchCfg, testUsers, testUserMap, nil, logger)
		_, _, err := h(ctx, req, SearchInput{UserID: "sasha", Query: "hello"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown user_id")
	})

	t.Run("remove", func(t *testing.T) {
		h := removeHandler(store, testUsers, testUserMap, logger)
		_, _, err := h(ctx, req, RemoveInput{UserID: "alexander", ID: "some-id"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown user_id")
	})

	t.Run("empty user_id", func(t *testing.T) {
		h := addRecordHandler(store, fakeEmbedder{}, testUsers, testUserMap, nil, logger)
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

	h := addRecordHandler(store, fakeEmbedder{}, testUsers, testUserMap, nil, logger)
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

func TestParseEncryptionKey(t *testing.T) {
	t.Run("empty string returns nil", func(t *testing.T) {
		key, err := parseEncryptionKey("")
		require.NoError(t, err)
		assert.Nil(t, key)
	})

	t.Run("valid 64-char hex returns 32 bytes", func(t *testing.T) {
		raw := "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"
		key, err := parseEncryptionKey(raw)
		require.NoError(t, err)
		assert.Len(t, key, 32)
		assert.Equal(t, byte(1), key[0])
		assert.Equal(t, byte(32), key[31])
	})

	t.Run("wrong length rejected", func(t *testing.T) {
		_, err := parseEncryptionKey("deadbeef") // only 4 bytes
		require.Error(t, err)
		assert.Contains(t, err.Error(), "64 lowercase hex")
	})

	t.Run("invalid hex rejected", func(t *testing.T) {
		raw := "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
		_, err := parseEncryptionKey(raw)
		require.Error(t, err)
	})
}

// TestMCPServer_EncryptionRequired checks that when a user has Encryption:true,
// the handler rejects calls without a key.
func TestMCPServer_EncryptionRequired(t *testing.T) {
	store := newTestStore(t)
	logger := slog.New(slog.DiscardHandler)

	encUserMap := map[string]config.UserConfig{
		"archer": {ID: "archer", Encryption: true},
		"anna":   {ID: "anna"},
	}

	t.Run("add_record without key is rejected", func(t *testing.T) {
		h := addRecordHandler(store, fakeEmbedder{}, testUsers, encUserMap, nil, logger)
		_, _, err := h(context.Background(), &mcp.CallToolRequest{}, AddRecordInput{
			UserID:  "archer",
			Content: "hello",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "record_encryption_key is required")
	})

	t.Run("non-encrypted user works without key", func(t *testing.T) {
		h := addRecordHandler(store, fakeEmbedder{}, testUsers, encUserMap, nil, logger)
		_, out, err := h(context.Background(), &mcp.CallToolRequest{}, AddRecordInput{
			UserID:  "anna",
			Content: "anna's note",
		})
		require.NoError(t, err)
		assert.NotEmpty(t, out.ID)
	})

	// Regression test: record_encryption_key is documented as "ignored" for a
	// user without encryption enabled — a malformed value must not be
	// format-validated (and so must not reject the call) in that case.
	t.Run("non-encrypted user with malformed key still succeeds", func(t *testing.T) {
		h := addRecordHandler(store, fakeEmbedder{}, testUsers, encUserMap, nil, logger)
		_, out, err := h(context.Background(), &mcp.CallToolRequest{}, AddRecordInput{
			UserID:              "anna",
			Content:             "anna's other note",
			RecordEncryptionKey: "not-a-valid-key",
		})
		require.NoError(t, err)
		assert.NotEmpty(t, out.ID)
	})
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func TestUserLocation(t *testing.T) {
	minsk := mustLoadLocation(t, "Europe/Minsk")
	locations := map[string]*time.Location{"archer": minsk}

	assert.Equal(t, minsk, userLocation(locations, "archer"))
	assert.Equal(t, time.UTC, userLocation(locations, "anna")) // not configured — falls back to UTC
	assert.Equal(t, time.UTC, userLocation(nil, "archer"))     // no locations at all
}

func TestFormatUserTime(t *testing.T) {
	minsk := mustLoadLocation(t, "Europe/Minsk")
	at := time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC) // 11:00 UTC == 14:00 in Minsk (UTC+3)

	got := formatUserTime(at, minsk)
	assert.Equal(t, "2026-09-26T14:00:00+03:00", got)
}

func TestParseLocalDateTime(t *testing.T) {
	minsk := mustLoadLocation(t, "Europe/Minsk")

	t.Run("date and time", func(t *testing.T) {
		got, err := parseLocalDateTime("2026-09-26 14:00", minsk, false)
		require.NoError(t, err)
		assert.Equal(t, time.Date(2026, 9, 26, 11, 0, 0, 0, time.UTC), got)
	})

	t.Run("date only, start of day", func(t *testing.T) {
		got, err := parseLocalDateTime("2026-09-26", minsk, false)
		require.NoError(t, err)
		assert.Equal(t, time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC), got) // 2026-09-26 00:00 Minsk
	})

	t.Run("date only, end of day", func(t *testing.T) {
		got, err := parseLocalDateTime("2026-09-26", minsk, true)
		require.NoError(t, err)
		assert.Equal(t, time.Date(2026, 9, 26, 20, 59, 59, 999999999, time.UTC), got) // 2026-09-26 23:59:59.999999999 Minsk
	})

	t.Run("invalid format rejected", func(t *testing.T) {
		_, err := parseLocalDateTime("not-a-date", minsk, false)
		require.Error(t, err)
	})
}

func TestResolveTimeRange(t *testing.T) {
	t.Run("both empty returns nil", func(t *testing.T) {
		tr, err := resolveTimeRange("test", "", "", time.UTC)
		require.NoError(t, err)
		assert.Nil(t, tr)
	})

	t.Run("both set", func(t *testing.T) {
		tr, err := resolveTimeRange("test", "2026-09-26 14:00", "2026-09-26 16:00", time.UTC)
		require.NoError(t, err)
		require.NotNil(t, tr.From)
		require.NotNil(t, tr.To)
		assert.Equal(t, time.Date(2026, 9, 26, 14, 0, 0, 0, time.UTC), *tr.From)
		assert.Equal(t, time.Date(2026, 9, 26, 16, 0, 0, 0, time.UTC), *tr.To)
	})

	t.Run("invalid from is a search-scoped error", func(t *testing.T) {
		_, err := resolveTimeRange("search", "garbage", "", time.UTC)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "search: from")
	})
}

// TestMCPServer_SearchTimeRangeUsesUserTimezone is the regression test for
// the core ask: a caller who says "records from X to Y" means X/Y in the
// searching user's own local timezone, not UTC, and results must come back
// timestamped in that same local timezone with the offset visible — not
// silently in UTC. Europe/Minsk has been a fixed UTC+3 offset with no DST
// since 2011, so asserting "+03:00" here isn't date-dependent.
func TestMCPServer_SearchTimeRangeUsesUserTimezone(t *testing.T) {
	store := newTestStore(t)
	logger := slog.New(slog.DiscardHandler)
	searchCfg := config.SearchConfig{DefaultLimit: 10, MaxLimit: 50}
	minsk := mustLoadLocation(t, "Europe/Minsk")
	locations := map[string]*time.Location{"archer": minsk}

	ctx := context.Background()
	addH := addRecordHandler(store, fakeEmbedder{}, testUsers, testUserMap, locations, logger)
	_, addOut, err := addH(ctx, &mcp.CallToolRequest{}, AddRecordInput{UserID: "archer", Content: "entry now"})
	require.NoError(t, err)
	assert.Contains(t, addOut.CreatedAt, "+03:00")

	searchH := searchHandler(store, fakeEmbedder{}, searchCfg, testUsers, testUserMap, locations, logger)

	t.Run("window containing now matches", func(t *testing.T) {
		nowMinsk := time.Now().In(minsk)
		from := nowMinsk.Add(-1 * time.Hour).Format("2006-01-02 15:04")
		to := nowMinsk.Add(1 * time.Hour).Format("2006-01-02 15:04")
		_, out, err := searchH(ctx, &mcp.CallToolRequest{}, SearchInput{UserID: "archer", Query: "entry", From: from, To: to})
		require.NoError(t, err)
		require.Len(t, out.Results, 1)
		assert.Contains(t, out.Results[0].CreatedAt, "+03:00")
	})

	t.Run("window excluding now returns nothing", func(t *testing.T) {
		nowMinsk := time.Now().In(minsk)
		from := nowMinsk.Add(-3 * time.Hour).Format("2006-01-02 15:04")
		to := nowMinsk.Add(-2 * time.Hour).Format("2006-01-02 15:04")
		_, out, err := searchH(ctx, &mcp.CallToolRequest{}, SearchInput{UserID: "archer", Query: "entry", From: from, To: to})
		require.NoError(t, err)
		assert.Len(t, out.Results, 0)
	})
}
