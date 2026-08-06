# miranda-diary — project notes for Claude Code

A personal diary MCP server: stores notes, thoughts, and events in SQLite
with Gemini vector embeddings for semantic search. Sibling project to
[miranda-code-execution-sandbox](../miranda-code-execution-sandbox) —
same stack, same conventions.

## Conventions

Same as miranda-code-execution-sandbox: write explanatory comments (doc-comments
on exported symbols, comments on non-obvious logic). The terse/no-comments default
doesn't apply here — a small home-infra codebase maintained intermittently benefits
more from comments that carry forward *why* a decision was made. Config follows
the same `Default()`-in-Go + YAML-overrides pattern as
[miranda-service-skeleton](../miranda-service-skeleton) — see Configuration below.

## Architecture

```
Miranda <--Streamable HTTP (bearer token)--> httpserver
                                                  |
                                             mcpserver
                                           /     |     \
                              add_record  /  search \  remove
                                         |           |
                                     diary.Store (SQLite)
                                         |
                                   embedding.Embedder
                                         |
                                  Gemini HTTP API
                                (gemini-embedding-2)
```

**The binary runs host-native** — no Docker, no CGO. `CGO_ENABLED=0`,
static binary, deployed and run the same way as miranda-code-execution-sandbox.
`modernc.org/sqlite` is used instead of `mattn/go-sqlite3` precisely because
it's a pure-Go SQLite port that doesn't need CGO.

### Configuration

Follows [miranda-service-skeleton](../miranda-service-skeleton)'s pattern
exactly: `internal/config.Default()` populates every field, and
`config/config.yaml.dist` is the single, checked-into-git source of truth
documenting every available field and its default — it is never loaded by
the running service. A real deployment provides any number of real
`config/*.yaml` files (a single `config.yaml`, or split by topic), each
gitignored (only `config.yaml.dist` is tracked). `main.go`'s
`configFilePaths` lists them (`os.ReadDir` over `config/` or the directory
named by `DIARY_CONFIG_DIR`, not `filepath.Glob` — see that function's own
comment for why) and passes the resulting paths, in that order, to
`config.Load(paths...)`, which starts from `Default()` and unmarshals each
file on top of it in turn — later files override earlier ones
field-by-field. A missing file or missing config directory is not an error;
`validate()` at the end of `Load()` is what rejects a config that's missing
something required (like `users` — see User isolation below).

### Request flow for `diary_search` (exposed as `search`)

1. `internal/httpserver.requireBearerToken` checks `Authorization: Bearer`
   against `auth_token_env`'s value — same constant-time comparison as sandbox.
2. `mcpserver` validates `user_id` and `query` are non-empty, clamps `limit`.
3. `embedding.GeminiEmbedder.Embed` calls the Gemini API to get a 768-dim
   float32 vector for the query text.
4. `diary.Store.Search` runs `SELECT ... FROM records WHERE user_id = ?`,
   loads all of that user's embeddings into memory, and ranks them by cosine
   similarity computed in Go.
5. Top `limit` results are returned sorted descending by score.

`add_record` is the same but writes: embed → INSERT.
`remove` is `DELETE FROM records WHERE id = ? AND user_id = ?`.

### User isolation

Every record has a `user_id TEXT NOT NULL` column. All three tool handlers
receive `user_id` as an explicit parameter from the MCP caller (Miranda), and
every SQL query filters by it. `remove` uses `AND user_id = ?` so a
caller can't delete another user's record even with a known ID — it just gets
`deleted: false`, indistinguishable from a missing record.

**Why user_id in the tool call, not derived from the token:** There's a single
shared bearer token (Miranda is the only caller). Miranda knows from conversation
context who it's talking to and passes `user_id` explicitly. This is simpler than
per-user tokens and sets up the right structure for the planned future addition of
per-user biometric encryption — at that point `user_id` → decryption key, and the
tool call already carries it.

**`user_id` is validated against `config.Config.Users`, not free text.**
Every tool handler runs `mcpserver.resolveUser` before touching the store —
an empty or unrecognized `user_id` is a hard error, not a value the store
silently accepts. This was added after a real incident: Miranda's system
prompt used to only tell the LLM the current speaker's *display name*, not
the technical id it must pass as `user_id`; for one household member the two
happened to coincide well enough by lexical accident that the bug went
unnoticed, for another they didn't, and food/diary tool calls silently
landed under the wrong person's data with no error at any layer. The fix is
two-layered: Miranda's own prompt now spells out the id explicitly (see
Miranda's `internal/httpapi/agent_loop.go`), and this validation exists so
that *if* a caller ever gets it wrong again anyway — a typo, a stale prompt,
a different future caller — it fails loudly instead of silently starting a
new, unsearchable `user_id` bucket. `users` has no built-in default (see
`config.Default`) specifically so a deployment can't accidentally run with
no allowlist at all; it must be set explicitly in the server's own
`config.yaml` (never committed with real household member ids — see
`config/config.yaml.dist`'s own template comment). `Users` is
`[]config.UserConfig` (a `yaml:"id"` struct field) rather than a bare
`[]string` specifically so per-user settings — the planned per-user
biometric encryption key reference, for one — can be added as new struct
fields later without another breaking rename of the config key.

### Embedding storage

Embeddings are stored as raw binary BLOBs in SQLite:
`encodeEmbedding` / `decodeEmbedding` in `internal/diary/store.go` pack
float32 values as little-endian 4-byte units. This is cheaper than JSON and
avoids any parsing overhead at search time.

Cosine similarity is computed in pure Go over all of a user's embeddings
loaded into memory. For a personal diary even at tens of thousands of entries
this is fast enough (10 000 records × 768 dims × 4 bytes ≈ 30 MB RAM,
similarity computation is a tight float multiply-accumulate loop). Don't
add a vector database unless profiling shows this is actually the bottleneck.

**Important:** if you switch the embedding model, the new model's embedding
space is incompatible with existing stored embeddings — similarity scores
become meaningless. There's no migration path built in yet; changing
`embedding.model` in config on a non-empty database requires re-embedding
all records manually (or wiping and re-adding).

### Gemini embedding model

`gemini-embedding-2` - very good
multilingual quality. The client is created once at startup in
`embedding.NewGemini` and reused across calls (the `genai.Client` is safe for
concurrent use). A nil API key is caught at startup, not at first call.

## Testing

```bash
make test        # go test ./... -race
```

`internal/diary/store_test.go` uses an in-memory SQLite database (`:memory:`)
so tests are fast and require no filesystem setup. Embedding tests would
require a real Gemini API key — there are none currently; the `Embedder`
interface exists to make the mcpserver testable with a fake embedder if needed.

`TestStore_UserIsolation` is the critical test: it verifies that one user
cannot see or delete another user's records, and that `Remove` with a
wrong `user_id` returns `deleted: false` without error.

## Deploying

`scripts/deploy.sh` cross-compiles for `linux/amd64` and deploys to
`archer@192.168.1.50` as a `systemd --user` service on port `:8789`.
`config/*.yaml` (everything except the tracked `config.yaml.dist` template)
and `.env` are **never touched by deploy** — they live on the server and
hold secrets or deployment-specific details. On first deploy, create
`~/miranda-diary/.env` manually:

```
DIARY_MCP_TOKEN=<openssl rand -hex 32>
GEMINI_API_KEY=<from Google AI Studio>
```

The service refuses to start if either is unset. Check `journalctl --user -u miranda-diary`
if the service doesn't come up — the error message names the missing variable.

## What's deliberately not here

- **Per-user tokens** — a single shared token is enough; Miranda is the only
  caller and user identity comes from the tool parameter, not the token.
- **Vector database (Qdrant, etc.)** — in-memory cosine similarity over SQLite
  BLOBs is sufficient for personal-diary scale and eliminates an external
  service dependency.
- **Ollama / local embedding** — Gemini free tier is simpler to operate than
  a local model; no GPU or extra process required.
- **Encryption at rest** — planned as a follow-up feature using per-user
  biometric keys. The `user_id` parameter is already in every tool call to
  support this; the schema and query structure don't need to change.
