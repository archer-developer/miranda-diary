// Command miranda-diary is a personal diary MCP server. It stores free-form
// notes, thoughts, and events in SQLite with Gemini-generated embeddings for
// semantic search. Three MCP tools are exposed — add_record, search, remove —
// over Streamable HTTP behind a bearer token.
//
// User isolation is enforced at the database level: every record is tagged with
// a user_id supplied by the caller (Miranda), and all queries filter by it.
// Authentication is a single shared token — Miranda is the only caller and
// already has it. Future versions will add per-user encryption keyed by
// biometric credentials.
//
// Architecture mirrors miranda-code-execution-sandbox: static CGO-free binary,
// config.yaml + .env for local development, systemd --user service on the server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-diary/internal/config"
	"github.com/archer-developer/miranda-diary/internal/diary"
	"github.com/archer-developer/miranda-diary/internal/embedding"
	"github.com/archer-developer/miranda-diary/internal/envfile"
	"github.com/archer-developer/miranda-diary/internal/httpserver"
	"github.com/archer-developer/miranda-diary/internal/mcpserver"
)

const (
	dotEnvPath        = ".env"
	defaultConfigPath = "config/config.yaml"
	shutdownTimeout   = 10 * time.Second
	debugLogDir       = "logs"
	debugLogFile      = "debug.log"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := envfile.Load(dotEnvPath); err != nil {
		logger.Warn("failed to load .env, continuing with process environment", "error", err)
	}

	cfgPath := defaultConfigPath
	if v := os.Getenv("DIARY_CONFIG"); v != "" {
		cfgPath = v
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}

	realLogger, closeLogger, err := buildLogger(cfg.Logging)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
	defer closeLogger()
	logger = realLogger

	if err := run(cfg, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, logger *slog.Logger) error {
	token := os.Getenv(cfg.AuthTokenEnv)
	if token == "" {
		return fmt.Errorf("main: environment variable %s (named by auth_token_env) is not set — refusing to start with no auth token", cfg.AuthTokenEnv)
	}

	apiKey := os.Getenv(cfg.Embedding.APIKeyEnv)
	if apiKey == "" {
		return fmt.Errorf("main: environment variable %s (named by embedding.api_key_env) is not set — Gemini API key required", cfg.Embedding.APIKeyEnv)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0o755); err != nil {
		return fmt.Errorf("main: create database directory: %w", err)
	}

	store, err := diary.New(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("main: open diary store: %w", err)
	}
	defer func() {
		logger.Info("closing diary store")
		_ = store.Close()
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	embedder, err := embedding.NewGemini(ctx, apiKey, cfg.Embedding.Model)
	if err != nil {
		return fmt.Errorf("main: init gemini embedder: %w", err)
	}

	server := mcpserver.New(store, embedder, cfg.Search, logger)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	handler := httpserver.New(mcpHandler, token)
	httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: handler}

	logger.Info("diary ready",
		"path", cfg.Database.Path,
		"embedding_model", cfg.Embedding.Model,
		"addr", cfg.HTTPAddr,
	)

	return serveUntilInterrupted(ctx, httpServer, logger)
}

func serveUntilInterrupted(ctx context.Context, httpServer *http.Server, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", httpServer.Addr)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

func buildLogger(cfg config.LoggingConfig) (*slog.Logger, func(), error) {
	noop := func() {}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, noop, fmt.Errorf("main: invalid logging.level %q: %w", cfg.Level, err)
	}

	stdoutLevel := level
	if stdoutLevel < slog.LevelInfo {
		stdoutLevel = slog.LevelInfo
	}
	stdoutHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: stdoutLevel})

	if level > slog.LevelDebug {
		return slog.New(stdoutHandler), noop, nil
	}

	if err := os.MkdirAll(debugLogDir, 0o755); err != nil {
		return nil, noop, fmt.Errorf("main: create debug log dir: %w", err)
	}
	debugPath := filepath.Join(debugLogDir, debugLogFile)
	debugWriter, err := os.OpenFile(debugPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, noop, fmt.Errorf("main: open debug log %s: %w", debugPath, err)
	}
	debugHandler := slog.NewTextHandler(debugWriter, &slog.HandlerOptions{Level: slog.LevelDebug})

	handler := &levelSplitHandler{stdout: stdoutHandler, debugFile: debugHandler}
	return slog.New(handler), func() { _ = debugWriter.Close() }, nil
}

type levelSplitHandler struct {
	stdout    slog.Handler
	debugFile slog.Handler
}

func (h *levelSplitHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.stdout.Enabled(ctx, level) || h.debugFile.Enabled(ctx, level)
}

func (h *levelSplitHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < slog.LevelInfo {
		return h.debugFile.Handle(ctx, r)
	}
	return h.stdout.Handle(ctx, r)
}

func (h *levelSplitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelSplitHandler{
		stdout:    h.stdout.WithAttrs(attrs),
		debugFile: h.debugFile.WithAttrs(attrs),
	}
}

func (h *levelSplitHandler) WithGroup(name string) slog.Handler {
	return &levelSplitHandler{
		stdout:    h.stdout.WithGroup(name),
		debugFile: h.debugFile.WithGroup(name),
	}
}
