// Package mcpserver registers the three diary MCP tools — add_record,
// search, remove — and wires them to the diary store and embedder.
package mcpserver

import (
	"context"
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

	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "add_record",
		Description: "Save a thought, event, note, or any piece of information to the personal diary. " +
			"The record is stored with a semantic embedding so it can be found later by meaning, not just keywords. " +
			"Use tags to group related entries (e.g. [\"work\", \"idea\"])." +
			userHint +
			" Returns the record ID and timestamp of the saved entry.",
	}, addRecordHandler(store, embedder, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "search",
		Description: "Search the personal diary by meaning. " +
			"Pass a natural-language query — the search finds semantically similar entries even when they use different words. " +
			"Returns matching records ranked by relevance with their content, tags, date, and similarity score. " +
			"Use limit to control how many results to return (default: " + fmt.Sprint(cfg.DefaultLimit) + ", max: " + fmt.Sprint(cfg.MaxLimit) + ")." +
			userHint,
	}, searchHandler(store, embedder, cfg, users, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "remove",
		Description: "Delete a diary record by its ID. " +
			"The ID is returned by add_record and appears in search results. " +
			"Returns whether a record was actually deleted (false means the ID was not found)." +
			userHint,
	}, removeHandler(store, users, logger))

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

// --- diary_add_record ---

type AddRecordInput struct {
	UserID  string   `json:"user_id" jsonschema:"The identifier of the user whose diary to write to. Must be one of the configured users."`
	Content string   `json:"content" jsonschema:"The text content of the diary entry — a thought, event, note, or any information to remember."`
	Tags    []string `json:"tags,omitempty" jsonschema:"Optional list of tags to categorize the entry, e.g. [\"work\", \"idea\", \"meeting\"]."`
}

type AddRecordOutput struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

func addRecordHandler(store *diary.Store, embedder embedding.Embedder, users []string, logger *slog.Logger) mcp.ToolHandlerFor[AddRecordInput, AddRecordOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in AddRecordInput) (*mcp.CallToolResult, AddRecordOutput, error) {
		userID, err := resolveUser(users, "add_record", in.UserID)
		if err != nil {
			return nil, AddRecordOutput{}, err
		}
		if strings.TrimSpace(in.Content) == "" {
			return nil, AddRecordOutput{}, fmt.Errorf("add_record: content must not be empty")
		}

		emb, err := embedder.Embed(ctx, in.Content)
		if err != nil {
			return nil, AddRecordOutput{}, fmt.Errorf("add_record: generate embedding: %w", err)
		}

		rec, err := store.Add(ctx, userID, in.Content, in.Tags, emb)
		if err != nil {
			return nil, AddRecordOutput{}, fmt.Errorf("add_record: store record: %w", err)
		}

		logger.Info("add_record",
			"user_id", userID,
			"id", rec.ID,
			"content_bytes", len(in.Content),
			"tags", in.Tags,
		)

		out := AddRecordOutput{
			ID:        rec.ID,
			CreatedAt: rec.CreatedAt.Format(time.RFC3339),
		}
		text := fmt.Sprintf("Saved. id: %s  created_at: %s", out.ID, out.CreatedAt)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, out, nil
	}
}

// --- diary_search ---

type SearchInput struct {
	UserID string `json:"user_id" jsonschema:"The identifier of the user whose diary to search. Must be one of the configured users."`
	Query  string `json:"query" jsonschema:"Natural-language search query. The search finds diary entries that are semantically similar to this text."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of results to return. Defaults to the server's configured default; clamped to the server's maximum."`
}

type SearchOutput struct {
	Results []SearchResultItem `json:"results"`
	Total   int                `json:"total"`
}

type SearchResultItem struct {
	ID        string   `json:"id"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	CreatedAt string   `json:"created_at"`
	Score     float64  `json:"score"`
}

func searchHandler(store *diary.Store, embedder embedding.Embedder, cfg config.SearchConfig, users []string, logger *slog.Logger) mcp.ToolHandlerFor[SearchInput, SearchOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
		userID, err := resolveUser(users, "search", in.UserID)
		if err != nil {
			return nil, SearchOutput{}, err
		}
		if strings.TrimSpace(in.Query) == "" {
			return nil, SearchOutput{}, fmt.Errorf("search: query must not be empty")
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

		results, err := store.Search(ctx, userID, emb, limit)
		if err != nil {
			return nil, SearchOutput{}, fmt.Errorf("search: store: %w", err)
		}

		logger.Info("search",
			"user_id", userID,
			"query_len", len(in.Query),
			"limit", limit,
			"found", len(results),
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

		out := SearchOutput{Results: items, Total: len(items)}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatSearchResults(items)}}}, out, nil
	}
}

func formatSearchResults(items []SearchResultItem) string {
	if len(items) == 0 {
		return "No matching diary entries found."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Found %d result(s):\n", len(items))
	for i, item := range items {
		fmt.Fprintf(&b, "\n--- #%d (id: %s, score: %.3f, date: %s) ---\n", i+1, item.ID, item.Score, item.CreatedAt)
		if len(item.Tags) > 0 {
			fmt.Fprintf(&b, "Tags: [%s]\n", strings.Join(item.Tags, ", "))
		}
		b.WriteString(item.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// --- diary_remove ---

type RemoveInput struct {
	UserID string `json:"user_id" jsonschema:"The identifier of the user who owns the record. Must be one of the configured users."`
	ID     string `json:"id" jsonschema:"The ID of the diary record to delete, as returned by add_record or search."`
}

type RemoveOutput struct {
	Deleted bool `json:"deleted"`
}

func removeHandler(store *diary.Store, users []string, logger *slog.Logger) mcp.ToolHandlerFor[RemoveInput, RemoveOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in RemoveInput) (*mcp.CallToolResult, RemoveOutput, error) {
		userID, err := resolveUser(users, "remove", in.UserID)
		if err != nil {
			return nil, RemoveOutput{}, err
		}
		if strings.TrimSpace(in.ID) == "" {
			return nil, RemoveOutput{}, fmt.Errorf("remove: id must not be empty")
		}

		deleted, err := store.Remove(ctx, userID, in.ID)
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
