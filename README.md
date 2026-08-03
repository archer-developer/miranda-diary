# miranda-diary

An MCP server that gives Miranda three tools for a personal diary:
`diary_add_record`, `diary_search`, `diary_remove`. Notes and thoughts are
stored in SQLite alongside semantic embeddings (Gemini `text-embedding-004`)
so they can be retrieved by meaning, not just keywords. It's a standalone Go
service — a sibling to [Miranda](../miranda) and
[miranda-code-execution-sandbox](../miranda-code-execution-sandbox) — meant to
be wired into Miranda as one more tool source.

```
Miranda <--Streamable HTTP--> miranda-diary <--Gemini API--> text-embedding-004
                                    |
                              SQLite (diary.db)
                          user_id + content + embedding
```

Records are never surfaced in Miranda's system prompt on their own —
`diary_search` must be called explicitly to pull them into context. Each
record is tagged with a `user_id` (supplied by Miranda from the conversation
context), and all queries filter by it so users only ever see their own
entries.

## Building

Requires Go 1.25+ (with `GOTOOLCHAIN=auto` — the default — `go build`
fetches a matching toolchain automatically). No Docker, no CGO.

```bash
go build -o miranda-diary ./cmd/miranda-diary
# or
make build
```

Cross-compiling for the deploy target:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o miranda-diary-linux-amd64 ./cmd/miranda-diary
```

## Running

```bash
cp .env.example .env
# fill in DIARY_MCP_TOKEN and GEMINI_API_KEY in .env

make run
```

The server listens on `:8789` by default: `GET /healthz` (unauthenticated)
and `/mcp` (the MCP endpoint, requires `Authorization: Bearer <DIARY_MCP_TOKEN>`).

## Testing

```bash
make test          # go test ./... -race
make lint          # golangci-lint run ./...
make fmt           # gofmt + goimports
make check         # fmt + lint + test — run this before committing
```

`make lint`/`make check` need `golangci-lint` and `goimports` on `PATH` —
`make tools` installs both.

## Deploying

```bash
./scripts/deploy.sh
```

Cross-compiles for `linux/amd64`, ships the binary over SSH, and restarts
the `systemd --user` service. `config/config.yaml` and `.env` are **never
uploaded** — they're managed on the server separately and hold secrets that
shouldn't be in the deploy flow.

On first deploy, create `~/miranda-diary/.env` on the server manually:

```bash
ssh archer@192.168.1.50 'mkdir -p ~/miranda-diary'
# then create ~/miranda-diary/.env with DIARY_MCP_TOKEN and GEMINI_API_KEY
```

## Configuration

Every field has a built-in default (see `internal/config.Default()`), so
`config/config.yaml` only needs to override what differs. The file shipped
in this repo has all fields commented out — the defaults run the service as-is.

```yaml
http_addr: ":8789"
auth_token_env: "DIARY_MCP_TOKEN"

database:
  path: "data/diary.db"

embedding:
  api_key_env: "GEMINI_API_KEY"
  model: "text-embedding-004"  # 768 dims, free tier (1 500 req/day)

search:
  default_limit: 10
  max_limit: 50

logging:
  level: "info"
```

Auth tokens and API keys are never stored in `config.yaml` — only the
**name** of the environment variable to read them from. The server refuses to
start if either is unset or empty.

### Gemini free tier

`text-embedding-004` is available on Google AI's free plan: 1 500 embedding
requests per day, no cost. For a personal diary that's effectively unlimited.
Get an API key at <https://aistudio.google.com/apikey>.

### Debug logging

Set `logging.level: "debug"` to log every tool call with full parameters.
Debug records go to `logs/debug.log` instead of stdout to avoid flooding the
systemd journal with potentially sensitive content — same split as
`miranda-code-execution-sandbox`.

## Wiring into Miranda

Add an entry to Miranda's `config/mcp.yaml`:

```yaml
mcp:
  servers:
    - name: diary
      url: "http://192.168.1.50:8789/mcp"
      token_env: "DIARY_MCP_TOKEN"
      enabled: true
```

and set `DIARY_MCP_TOKEN` in Miranda's `.env` to the same value configured
here. Miranda will then expose the tools as `diary_diary_add_record`,
`diary_diary_search`, and `diary_diary_remove`.

## MCP tools

### `diary_add_record`

Saves a diary entry and returns its ID and timestamp. The content is
embedded via Gemini and stored alongside it so future searches can find it
by meaning.

```json
{
  "user_id": "alexander",
  "content": "Met with the team today to discuss the Q3 roadmap. Key decision: prioritise the billing rewrite.",
  "tags": ["work", "meeting", "q3"]
}
```

### `diary_search`

Semantic search over one user's diary. Returns records ranked by cosine
similarity to the query — finds related entries even when they use different
words.

```json
{
  "user_id": "alexander",
  "query": "billing system decisions",
  "limit": 5
}
```

### `diary_remove`

Deletes a record by ID. Only succeeds if the record belongs to the given
`user_id` — a wrong `user_id` returns `deleted: false` without revealing
that the ID exists.

```json
{
  "user_id": "alexander",
  "id": "550e8400-e29b-41d4-a716-446655440000"
}
```

## User isolation

All three tools take a `user_id` parameter. Miranda supplies it from
conversation context (it knows who it's talking to). Isolation is enforced
at the database level — every SQL query carries `WHERE user_id = ?` — so
one user's records are never visible to another, even if they share the same
service instance and token.

Future versions will add per-user encryption keyed by biometric credentials,
so records at rest will be unreadable without the owner's key even to the
server operator.

## Project layout

```
cmd/miranda-diary/        entrypoint: config, wiring, HTTP listen, graceful shutdown
internal/config/          Default() + YAML config
internal/envfile/         .env loader
internal/diary/           SQLite store: Add, Search, Remove, Count + cosine similarity
internal/embedding/       Embedder interface + Gemini implementation
internal/mcpserver/       three MCP tools wired to store + embedder
internal/httpserver/      bearer-token auth + /healthz
scripts/deploy.sh         cross-compile + SSH deploy + systemd restart
```
