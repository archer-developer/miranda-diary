// Package mcpserver registers the three diary MCP tools — add_record,
// search, remove — and wires them to the diary store and embedder.
package mcpserver

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-diary/internal/config"
	"github.com/archer-developer/miranda-diary/internal/diary"
	"github.com/archer-developer/miranda-diary/internal/embedding"
)

const (
	serverName    = "miranda-diary"
	serverVersion = "0.1.0"
)

// New builds and returns the MCP server with all three diary tools registered.
// configuredUsers is the closed set of allowed household members
// (config.Config.Users) — every handler rejects a call whose user_id isn't
// one of their IDs, see resolveUser. A nil logger falls back to
// slog.Default().
func New(store *diary.Store, embedder embedding.Embedder, cfg config.SearchConfig, configuredUsers []config.UserConfig, logger *slog.Logger) *mcp.Server {
	if logger == nil {
		logger = slog.Default()
	}

	userIDs := make([]string, len(configuredUsers))
	for i, u := range configuredUsers {
		userIDs[i] = u.ID
	}
	users := slices.Sorted(slices.Values(userIDs))
	userHint := fmt.Sprintf(" user_id must be one of the configured users: %s.", strings.Join(users, ", "))

	// userMap provides O(1) lookup of per-user settings (e.g. Encryption) inside
	// handlers without re-scanning the slice on every tool call.
	userMap := make(map[string]config.UserConfig, len(configuredUsers))
	for _, u := range configuredUsers {
		userMap[u.ID] = u
	}

	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "add_record",
		Description: "Save a thought, event, note, or any piece of information to the personal diary. " +
			"The record is stored with a semantic embedding so it can be found later by meaning, not just keywords. " +
			"Use tags to group related entries (e.g. [\"work\", \"idea\"])." +
			userHint +
			" Returns the record ID and timestamp of the saved entry.",
	}, addRecordHandler(store, embedder, users, userMap, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "search",
		Description: "Search the personal diary by meaning. " +
			"Pass a natural-language query — the search finds semantically similar entries even when they use different words. " +
			"Returns matching records ranked by relevance with their content, tags, date, and similarity score. " +
			"Use limit to control how many results to return (default: " + fmt.Sprint(cfg.DefaultLimit) + ", max: " + fmt.Sprint(cfg.MaxLimit) + ")." +
			userHint,
	}, searchHandler(store, embedder, cfg, users, userMap, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "remove",
		Description: "Delete a diary record by its ID. " +
			"The ID is returned by add_record and appears in search results. " +
			"Returns whether a record was actually deleted (false means the ID was not found)." +
			userHint,
	}, removeHandler(store, users, userMap, logger))

	return server
}

// resolveUser trims userID and checks it against users (the sorted list
// New() already computed once). An empty or unrecognized user_id is a
// caller error, not something worth guessing a default for — see
// config.Config.Users for why this can't just accept anything.
func resolveUser(users []string, action, userID string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", fmt.Errorf("%s: user_id is required; configured users: %s", action, strings.Join(users, ", "))
	}
	if !slices.Contains(users, userID) {
		return "", fmt.Errorf("%s: unknown user_id %q; configured users: %s", action, userID, strings.Join(users, ", "))
	}
	return userID, nil
}

// parseEncryptionKey decodes a hex-encoded 32-byte encryption key. Returns nil
// when raw is empty (no key provided). Returns an error for any non-empty
// string that is not exactly 64 lowercase hex characters.
func parseEncryptionKey(raw string) ([]byte, error) {
	if raw == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("record_encryption_key must be 64 lowercase hex characters (32 bytes)")
	}
	return key, nil
}

// resolveEncryptionKey gates and parses raw (the caller-supplied
// record_encryption_key) against user's encryption setting. When encryption
// is disabled for user, raw is ignored entirely — not even format-checked —
// matching the field's documented "ignored otherwise" contract. When enabled,
// raw must decode to a valid key or the call is rejected.
func resolveEncryptionKey(action string, user config.UserConfig, raw string) ([]byte, error) {
	if !user.Encryption {
		return nil, nil
	}
	keyBytes, err := parseEncryptionKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if keyBytes == nil {
		return nil, fmt.Errorf("%s: record_encryption_key is required for user %q (encryption is enabled)", action, user.ID)
	}
	return keyBytes, nil
}

// --- add_record ---

type AddRecordInput struct {
	UserID  string   `json:"user_id" jsonschema:"The identifier of the user whose diary to write to. Must be one of the configured users."`
	Content string   `json:"content" jsonschema:"The text content of the diary entry — a thought, event, note, or any information to remember."`
	Tags    []string `json:"tags,omitempty" jsonschema:"Optional list of tags to categorize the entry, e.g. [\"work\", \"idea\", \"meeting\"]."`
	// RecordEncryptionKey is the AES-256 key for this user's diary records,
	// hex-encoded (64 lowercase hex characters = 32 bytes). Miranda provides
	// this automatically based on the authenticated user's session key. Required
	// when encryption is enabled for the user in server config; ignored otherwise.
	// The field name record_encryption_key is the agreed trigger for Miranda's
	// encryption-key injection whitelist — do not rename it.
	RecordEncryptionKey string `json:"record_encryption_key,omitempty" jsonschema:"AES-256 encryption key for this user's diary records, hex-encoded (64 lowercase hex characters = 32 bytes). Miranda provides this automatically from the user's authenticated session. Required when encryption is enabled for the user; ignored otherwise."`
}

type AddRecordOutput struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

func addRecordHandler(store *diary.Store, embedder embedding.Embedder, users []string, userMap map[string]config.UserConfig, logger *slog.Logger) mcp.ToolHandlerFor[AddRecordInput, AddRecordOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in AddRecordInput) (*mcp.CallToolResult, AddRecordOutput, error) {
		// Scrub before any other handler code touches in, so a future change
		// that logs the input struct directly can never leak the raw key.
		rawKey := in.RecordEncryptionKey
		in.RecordEncryptionKey = ""

		userID, err := resolveUser(users, "add_record", in.UserID)
		if err != nil {
			return nil, AddRecordOutput{}, err
		}
		if strings.TrimSpace(in.Content) == "" {
			return nil, AddRecordOutput{}, fmt.Errorf("add_record: content must not be empty")
		}

		user := userMap[userID]
		keyBytes, err := resolveEncryptionKey("add_record", user, rawKey)
		if err != nil {
			return nil, AddRecordOutput{}, err
		}

		// Verify the key before the expensive embedding call, so a wrong key
		// fails fast without burning a Gemini API round-trip.
		if keyBytes != nil {
			if err := store.VerifyEncryptionKey(ctx, userID, keyBytes); err != nil {
				return nil, AddRecordOutput{}, fmt.Errorf("add_record: %w", err)
			}
		}

		emb, err := embedder.Embed(ctx, in.Content)
		if err != nil {
			return nil, AddRecordOutput{}, fmt.Errorf("add_record: generate embedding: %w", err)
		}

		rec, err := store.Add(ctx, userID, in.Content, in.Tags, emb, keyBytes)
		if err != nil {
			return nil, AddRecordOutput{}, fmt.Errorf("add_record: store record: %w", err)
		}

		logger.Info("add_record",
			"user_id", userID,
			"id", rec.ID,
			"content_bytes", len(in.Content),
			"tags", in.Tags,
			"encrypted", user.Encryption,
		)

		out := AddRecordOutput{
			ID:        rec.ID,
			CreatedAt: rec.CreatedAt.Format(time.RFC3339),
		}
		text := fmt.Sprintf("Saved. id: %s  created_at: %s", out.ID, out.CreatedAt)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- search ---

type SearchInput struct {
	UserID              string `json:"user_id" jsonschema:"The identifier of the user whose diary to search. Must be one of the configured users."`
	Query               string `json:"query" jsonschema:"Natural-language search query. The search finds diary entries that are semantically similar to this text."`
	Limit               int    `json:"limit,omitempty" jsonschema:"Maximum number of results to return. Defaults to the server's configured default; clamped to the server's maximum."`
	RecordEncryptionKey string `json:"record_encryption_key,omitempty" jsonschema:"AES-256 encryption key for this user's diary records, hex-encoded (64 lowercase hex characters = 32 bytes). Miranda provides this automatically from the user's authenticated session. Required when encryption is enabled for the user; ignored otherwise."`
}

type SearchOutput struct {
	Results []SearchResultItem `json:"results"`
	Total   int                `json:"total"`
	// SkippedEncrypted counts records that were not searched because they're
	// encrypted and no key was supplied (e.g. encryption was enabled and
	// later disabled for this user). Zero when nothing was skipped.
	SkippedEncrypted int `json:"skipped_encrypted,omitempty"`
}

type SearchResultItem struct {
	ID        string   `json:"id"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	CreatedAt string   `json:"created_at"`
	Score     float64  `json:"score"`
}

func searchHandler(store *diary.Store, embedder embedding.Embedder, cfg config.SearchConfig, users []string, userMap map[string]config.UserConfig, logger *slog.Logger) mcp.ToolHandlerFor[SearchInput, SearchOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
		rawKey := in.RecordEncryptionKey
		in.RecordEncryptionKey = ""

		userID, err := resolveUser(users, "search", in.UserID)
		if err != nil {
			return nil, SearchOutput{}, err
		}
		if strings.TrimSpace(in.Query) == "" {
			return nil, SearchOutput{}, fmt.Errorf("search: query must not be empty")
		}

		user := userMap[userID]
		keyBytes, err := resolveEncryptionKey("search", user, rawKey)
		if err != nil {
			return nil, SearchOutput{}, err
		}

		if keyBytes != nil {
			if err := store.VerifyEncryptionKey(ctx, userID, keyBytes); err != nil {
				return nil, SearchOutput{}, fmt.Errorf("search: %w", err)
			}
		}

		limit := in.Limit
		if limit <= 0 {
			limit = cfg.DefaultLimit
		}
		if limit > cfg.MaxLimit {
			limit = cfg.MaxLimit
		}

		emb, err := embedder.Embed(ctx, in.Query)
		if err != nil {
			return nil, SearchOutput{}, fmt.Errorf("search: generate query embedding: %w", err)
		}

		results, skipped, err := store.Search(ctx, userID, emb, limit, keyBytes)
		if err != nil {
			return nil, SearchOutput{}, fmt.Errorf("search: store: %w", err)
		}

		logger.Info("search",
			"user_id", userID,
			"query_len", len(in.Query),
			"limit", limit,
			"found", len(results),
			"skipped_encrypted", skipped,
		)

		items := make([]SearchResultItem, len(results))
		for i, r := range results {
			items[i] = SearchResultItem{
				ID:        r.ID,
				Content:   r.Content,
				Tags:      r.Tags,
				CreatedAt: r.CreatedAt.Format(time.RFC3339),
				Score:     r.Score,
			}
		}

		out := SearchOutput{Results: items, Total: len(items), SkippedEncrypted: skipped}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatSearchResults(items, skipped)}}}, out, nil
	}
}

func formatSearchResults(items []SearchResultItem, skippedEncrypted int) string {
	var b strings.Builder
	if len(items) == 0 {
		b.WriteString("No matching diary entries found.")
	} else {
		fmt.Fprintf(&b, "Found %d result(s):\n", len(items))
		for i, item := range items {
			fmt.Fprintf(&b, "\n--- #%d (id: %s, score: %.3f, date: %s) ---\n", i+1, item.ID, item.Score, item.CreatedAt)
			if len(item.Tags) > 0 {
				fmt.Fprintf(&b, "Tags: [%s]\n", strings.Join(item.Tags, ", "))
			}
			b.WriteString(item.Content)
			b.WriteString("\n")
		}
	}
	if skippedEncrypted > 0 {
		fmt.Fprintf(&b, "\nNote: %d further entries are encrypted and require your key; they were not searched.\n", skippedEncrypted)
	}
	return b.String()
}

// --- remove ---

type RemoveInput struct {
	UserID              string `json:"user_id" jsonschema:"The identifier of the user who owns the record. Must be one of the configured users."`
	ID                  string `json:"id" jsonschema:"The ID of the diary record to delete, as returned by add_record or search."`
	RecordEncryptionKey string `json:"record_encryption_key,omitempty" jsonschema:"AES-256 encryption key for this user's diary records, hex-encoded (64 lowercase hex characters = 32 bytes). Miranda provides this automatically from the user's authenticated session. Required when encryption is enabled for the user; ignored otherwise."`
}

type RemoveOutput struct {
	Deleted bool `json:"deleted"`
}

func removeHandler(store *diary.Store, users []string, userMap map[string]config.UserConfig, logger *slog.Logger) mcp.ToolHandlerFor[RemoveInput, RemoveOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in RemoveInput) (*mcp.CallToolResult, RemoveOutput, error) {
		rawKey := in.RecordEncryptionKey
		in.RecordEncryptionKey = ""

		userID, err := resolveUser(users, "remove", in.UserID)
		if err != nil {
			return nil, RemoveOutput{}, err
		}
		if strings.TrimSpace(in.ID) == "" {
			return nil, RemoveOutput{}, fmt.Errorf("remove: id must not be empty")
		}

		user := userMap[userID]
		keyBytes, err := resolveEncryptionKey("remove", user, rawKey)
		if err != nil {
			return nil, RemoveOutput{}, err
		}

		deleted, err := store.Remove(ctx, userID, in.ID, keyBytes)
		if err != nil {
			return nil, RemoveOutput{}, fmt.Errorf("remove: %w", err)
		}

		logger.Info("remove", "user_id", userID, "id", in.ID, "deleted", deleted)

		out := RemoveOutput{Deleted: deleted}
		var text string
		if deleted {
			text = fmt.Sprintf("Deleted record %s.", in.ID)
		} else {
			text = fmt.Sprintf("No record found with id %s.", in.ID)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}
