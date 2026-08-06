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
// Architecture mirrors miranda-service-skeleton and miranda-code-execution-sandbox:
// static CGO-free binary, config/*.yaml + .env for local development,
// systemd --user service on the server.
//
// Bootstrap: envfile.Load(.env) -> list config/*.yaml (configFilePaths) ->
// config.Load(paths...) -> build the real logger -> check required secrets
// are set -> build the MCP server -> wrap it in a Streamable HTTP handler ->
// mount it behind bearer auth and /healthz -> serve until SIGINT/SIGTERM,
// then shut down gracefully.
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
	"strings"
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
	dotEnvPath       = ".env"
	defaultConfigDir = "config"
	// configDirEnv overrides defaultConfigDir — e.g. for a deployment layout
	// that keeps config elsewhere.
	configDirEnv    = "DIARY_CONFIG_DIR"
	shutdownTimeout = 10 * time.Second
	debugLogDir     = "logs"
	debugLogFile    = "debug.log"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := envfile.Load(dotEnvPath); err != nil {
		logger.Warn("failed to load .env, continuing with process environment", "error", err)
	}

	cfgDir := defaultConfigDir
	if v := os.Getenv(configDirEnv); v != "" {
		cfgDir = v
	}

	configPaths, err := configFilePaths(cfgDir, logger)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}

	cfg, err := config.Load(configPaths...)
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

// configFilePaths lists the real config override files the service should
// load: every regular file directly under cfgDir whose name ends in
// ".yaml", sorted by name (os.ReadDir guarantees this) so config.Load's
// merge order — later file wins per field — is deterministic. This
// deliberately excludes config.yaml.dist, since it doesn't end in plain
// ".yaml"; see internal/config's doc comment for the full merge semantics.
//
// os.ReadDir is used instead of filepath.Glob deliberately: Glob treats
// cfgDir as pattern syntax (so a directory literally named e.g.
// "configs[2024]" silently matches nothing instead of being read as a
// literal path) and reports "no matches, no error" for a directory that
// doesn't exist *or* one that exists but can't be read (e.g. a permissions
// mistake) — indistinguishable from "no config files, use defaults" even
// though the latter is a real misconfiguration that should be loud.
// os.ReadDir never interprets cfgDir as a pattern and returns a real error
// for the unreadable case. A missing cfgDir is not treated as an error here
// either — it's the same as an empty one, i.e. "no overrides" — but is
// logged so a typo'd DIARY_CONFIG_DIR is at least visible in the log rather
// than silently producing an all-defaults service (which, since users has
// no default, fails startup anyway — see config.Load).
func configFilePaths(cfgDir string, logger *slog.Logger) ([]string, error) {
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Warn("config directory not found, using built-in defaults", "dir", cfgDir)
			return nil, nil
		}
		return nil, fmt.Errorf("main: read config dir %s: %w", cfgDir, err)
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		paths = append(paths, filepath.Join(cfgDir, entry.Name()))
	}

	logger.Info("config files discovered", "dir", cfgDir, "paths", paths)
	return paths, nil
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

	server := mcpserver.New(store, embedder, cfg.Search, cfg.Users, logger)
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
