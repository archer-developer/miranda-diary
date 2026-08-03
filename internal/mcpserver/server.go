// Package mcpserver registers the three diary MCP tools — diary_add_record,
// diary_search, diary_remove — and wires them to the diary store and embedder.
package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
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
// A nil logger falls back to slog.Default().
func New(store *diary.Store, embedder embedding.Embedder, cfg config.SearchConfig, logger *slog.Logger) *mcp.Server {
	if logger == nil {
		logger = slog.Default()
	}

	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: serverVersion}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name: "diary_add_record",
		Description: "Save a thought, event, note, or any piece of information to the personal diary. " +
			"The record is stored with a semantic embedding so it can be found later by meaning, not just keywords. " +
			"Use tags to group related entries (e.g. [\"work\", \"idea\"]). " +
			"Returns the record ID and timestamp of the saved entry.",
	}, addRecordHandler(store, embedder, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "diary_search",
		Description: "Search the personal diary by meaning. " +
			"Pass a natural-language query — the search finds semantically similar entries even when they use different words. " +
			"Returns matching records ranked by relevance with their content, tags, date, and similarity score. " +
			"Use limit to control how many results to return (default: " + fmt.Sprint(cfg.DefaultLimit) + ", max: " + fmt.Sprint(cfg.MaxLimit) + ").",
	}, searchHandler(store, embedder, cfg, logger))

	mcp.AddTool(server, &mcp.Tool{
		Name: "diary_remove",
		Description: "Delete a diary record by its ID. " +
			"The ID is returned by diary_add_record and appears in diary_search results. " +
			"Returns whether a record was actually deleted (false means the ID was not found).",
	}, removeHandler(store, logger))

	return server
}

// --- diary_add_record ---

type AddRecordInput struct {
	UserID  string   `json:"user_id" jsonschema:"The identifier of the user whose diary to write to (e.g. \"alexander\")."`
	Content string   `json:"content" jsonschema:"The text content of the diary entry — a thought, event, note, or any information to remember."`
	Tags    []string `json:"tags,omitempty" jsonschema:"Optional list of tags to categorize the entry, e.g. [\"work\", \"idea\", \"meeting\"]."`
}

type AddRecordOutput struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
}

func addRecordHandler(store *diary.Store, embedder embedding.Embedder, logger *slog.Logger) mcp.ToolHandlerFor[AddRecordInput, AddRecordOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in AddRecordInput) (*mcp.CallToolResult, AddRecordOutput, error) {
		if strings.TrimSpace(in.UserID) == "" {
			return nil, AddRecordOutput{}, fmt.Errorf("diary_add_record: user_id must not be empty")
		}
		if strings.TrimSpace(in.Content) == "" {
			return nil, AddRecordOutput{}, fmt.Errorf("diary_add_record: content must not be empty")
		}

		emb, err := embedder.Embed(ctx, in.Content)
		if err != nil {
			return nil, AddRecordOutput{}, fmt.Errorf("diary_add_record: generate embedding: %w", err)
		}

		rec, err := store.Add(ctx, in.UserID, in.Content, in.Tags, emb)
		if err != nil {
			return nil, AddRecordOutput{}, fmt.Errorf("diary_add_record: store record: %w", err)
		}

		logger.Info("diary_add_record",
			"user_id", in.UserID,
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
	UserID string `json:"user_id" jsonschema:"The identifier of the user whose diary to search (e.g. \"alexander\")."`
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

func searchHandler(store *diary.Store, embedder embedding.Embedder, cfg config.SearchConfig, logger *slog.Logger) mcp.ToolHandlerFor[SearchInput, SearchOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
		if strings.TrimSpace(in.UserID) == "" {
			return nil, SearchOutput{}, fmt.Errorf("diary_search: user_id must not be empty")
		}
		if strings.TrimSpace(in.Query) == "" {
			return nil, SearchOutput{}, fmt.Errorf("diary_search: query must not be empty")
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
			return nil, SearchOutput{}, fmt.Errorf("diary_search: generate query embedding: %w", err)
		}

		results, err := store.Search(ctx, in.UserID, emb, limit)
		if err != nil {
			return nil, SearchOutput{}, fmt.Errorf("diary_search: search: %w", err)
		}

		logger.Info("diary_search",
			"user_id", in.UserID,
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
	UserID string `json:"user_id" jsonschema:"The identifier of the user who owns the record (e.g. \"alexander\")."`
	ID     string `json:"id" jsonschema:"The ID of the diary record to delete, as returned by diary_add_record or diary_search."`
}

type RemoveOutput struct {
	Deleted bool `json:"deleted"`
}

func removeHandler(store *diary.Store, logger *slog.Logger) mcp.ToolHandlerFor[RemoveInput, RemoveOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in RemoveInput) (*mcp.CallToolResult, RemoveOutput, error) {
		if strings.TrimSpace(in.UserID) == "" {
			return nil, RemoveOutput{}, fmt.Errorf("diary_remove: user_id must not be empty")
		}
		if strings.TrimSpace(in.ID) == "" {
			return nil, RemoveOutput{}, fmt.Errorf("diary_remove: id must not be empty")
		}

		deleted, err := store.Remove(ctx, in.UserID, in.ID)
		if err != nil {
			return nil, RemoveOutput{}, fmt.Errorf("diary_remove: %w", err)
		}

		logger.Info("diary_remove", "user_id", in.UserID, "id", in.ID, "deleted", deleted)

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
