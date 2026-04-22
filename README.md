# ccproxy

OpenAI-compatible HTTP proxy in front of a locally-installed Claude Code CLI. Any client that speaks `POST /v1/chat/completions` — n8n, LibreChat, Open WebUI, the `openai` SDKs — can drive Claude Code over the network. Callers inherit the host's MCP servers, skills, subagents, and Bash tool access — that's the whole product.

## Status

**Actively in development.** ccproxy is pre-1.0 — the wire surface (headers, config schema, scopes) may still shift between minor versions. Expect breaking changes; pin a specific version (`go install …@v0.x.y`) if you deploy it, and read the commit log before upgrading. Bug reports and PRs welcome.

## Requirements

- Go 1.26+ (only for `go install` or building from source)
- A working `claude` binary on `$PATH` (or set `claude_binary:` in `config.yaml`)
- macOS or Linux

## Install

The fastest path — fetches the latest tagged release from GitHub, builds, and drops the binary into `$GOBIN` (or `$(go env GOPATH)/bin`):

```sh
go install github.com/guryn/ccproxy/cmd/ccproxy@latest
ccproxy version
```

Pin a specific version with `@v0.2.0`, or track main with `@main`. Make sure `$(go env GOPATH)/bin` is on your `$PATH`.

### From source

```sh
git clone https://github.com/guryn/ccproxy.git
cd ccproxy/sources            # Go module root
go build -o ccproxy ./cmd/ccproxy
./ccproxy version
```

## Quick start

Local dev loop, running from a cloned source tree:

```sh
# 1. set up env (defaults are dev-friendly)
cp .env.example .env

# 2. boot — first start auto-mints a bearer and prints it in a banner.
#    Copy the `ccp_...` value from the startup log; it is not recoverable.
ccproxy serve
```

Want a token with wider scopes (e.g. for session-persistent dev)?
Run `ccproxy token create --name dev --scopes 'chat,session:persistent,workspace:*'`
after first boot and rotate/revoke the auto-minted one.

`.env` is auto-loaded from CWD via `joho/godotenv`. Existing env vars always win.

If you're hacking on the code instead of running an installed binary, swap `ccproxy` for `go run ./cmd/ccproxy` in the commands above.

### Running an installed binary

`go install` drops `ccproxy` into `$GOBIN` but ships no config files — you pick the paths. ccproxy resolves `config.yaml` in this order: `--config <path>` → `$CCPROXY_CONFIG` → `$CCPROXY_STATE/config.yaml` → `./ccproxy.yaml`. State (`tokens.db`, `captures/`) lives in `$CCPROXY_STATE` (default `/var/lib/ccproxy`).

**Personal / single-user setup.** Create a state dir, drop a minimal `config.yaml` in it, export the relevant env vars in your shell rc (`~/.zshrc`, `~/.bashrc`):

```sh
mkdir -p ~/.config/ccproxy

cat > ~/.config/ccproxy/config.yaml <<'EOF'
allow_passthrough_models: true
default_claude_model: sonnet
EOF

# --- add to your shell rc ---
export CCPROXY_STATE=~/.config/ccproxy         # required: where tokens.db + captures/ live
# export CCPROXY_CONFIG=~/.config/ccproxy/config.yaml   # optional — picked up from $CCPROXY_STATE by default
export CCPROXY_LISTEN=127.0.0.1:4141           # optional: default is :4141 (all interfaces)
# export CCPROXY_METRICS_LISTEN=127.0.0.1:9101 # optional: enable /metrics on a separate port
# export CCPROXY_LOG_FORMAT=text               # optional: force text logs even when piped
# export CCPROXY_TOKEN=ccp_...                 # optional first-boot seed; leave empty to auto-mint

ccproxy serve                                   # banner prints the first bearer
```

That's enough for passthrough-only use (`model: "sonnet"` on the wire). Add workspaces and aliases later — see [`deploy/config.yaml.example`](deploy/config.yaml.example). Full env var reference is in the [Configuration](#configuration) section below.

**Server / systemd setup:** split config (declarative) from state (mutable):

```sh
sudo useradd --system --home /var/lib/ccproxy --shell /usr/sbin/nologin ccproxy
sudo mkdir -p /etc/ccproxy /var/lib/ccproxy
sudo chown ccproxy:ccproxy /var/lib/ccproxy
sudo cp config.yaml /etc/ccproxy/config.yaml
```

Then point the systemd unit at both — the template in `deploy/ccproxy.service` already uses `CCPROXY_STATE=/var/lib/ccproxy` and `CCPROXY_CONFIG=/etc/ccproxy/config.yaml`, plus an optional `EnvironmentFile=-/etc/ccproxy/env` for deploy-time overrides (listen addr, metrics listener). On first `systemctl start`, grep the journal for the banner:

```sh
journalctl -u ccproxy | grep -A5 "auto-minted"
```

**No `.env` in production.** `.env` is a dev-loop convenience — it's only read from CWD when you run `ccproxy serve` interactively. Under systemd, use `Environment=` / `EnvironmentFile=`; under Docker, use `environment:` in compose.

## Module layout

Everything below lives under `sources/` in the repo (the Go module root).

```
sources/
├── cmd/ccproxy/           # CLI entrypoint. Subcommands: serve | token | version
├── internal/
│   ├── auth/              # SQLite-backed token store, argon2id, scopes, in-memory cache
│   ├── claude/            # `claude -p --output-format stream-json` subprocess wrapper
│   ├── config/            # YAML loader (config.yaml schema + deploy-time env overrides)
│   ├── obs/               # Prometheus metrics + instrumentation middleware
│   ├── openai/            # OpenAI request/response/SSE types — knows nothing about Claude
│   ├── ratelimit/         # per-token rpm bucket + persistent daily token counter
│   ├── server/            # HTTP wiring, middleware, debug capture, graceful shutdown
│   ├── session/           # persistent session registry, TTL eviction, --resume plumbing
│   ├── translate/         # the only package that bridges OpenAI ↔ Claude. Verbosity modes live here.
│   └── workspace/         # PRD §6.4 resolution chain + ephemeral dir lifecycle
├── tests/integration/     # //go:build integration — real `claude` binary required
├── deploy/                # systemd unit, compose example, config template
├── Dockerfile             # container image; build from this dir
├── Makefile               # test / test-race / test-integration / build / release
└── LICENSE
```

**Boundary rule:** `openai/` and `claude/` know nothing about each other. `translate/` is the only bridge — keeps the door open for non-Claude backends without speculative abstraction.

## Common commands

```sh
make build                               # compile everything
make vet                                 # static checks
make test                                # unit test suite
make test-race                           # with race detector
make test-integration                    # real `claude` binary required
make release                             # static, stripped, trimpath release binary

# or the raw equivalents:
go test ./internal/server/... -v         # one package, verbose
go run ./cmd/ccproxy version             # print build info
go run ./cmd/ccproxy serve --help        # serve flag list
go run ./cmd/ccproxy token list          # admin
```

The release binary is a single static file; no CGO (the SQLite driver is `modernc.org/sqlite`).

## CLI subcommands

```
ccproxy serve [--config path] [--env-file path]
ccproxy token create --name <s> [--scopes chat,workspace:myproject] [--ttl 90d] [--rpm N] [--tpd N] [--default-verbosity verbose] [--debug-capture]
ccproxy token list
ccproxy token revoke <id-or-name>
ccproxy token rotate <id-or-name>
ccproxy token update <id-or-name> [--debug-capture on|off]
ccproxy version
```

## HTTP API

| Method | Path | Auth | Notes |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | yes | Streaming + non-streaming. OpenAI-compatible. |
| `GET`  | `/v1/models` | yes | Lists model aliases from config. |
| `GET`  | `/healthz` | no | Reports cached `claude --version`. |
| `GET`  | `/readyz` | no | 200 if `claude --version` succeeded at boot, 503 otherwise. |
| `GET`  | `/metrics` | no | **Separate listener** — only enabled when `CCPROXY_METRICS_LISTEN` is set. Do not expose publicly. |

### Request body

Standard OpenAI shape, plus ccproxy extensions:

```jsonc
{
  "model": "claude-code",                 // required (see /v1/models)
  "messages": [
    {"role": "system",    "content": "be terse"},
    {"role": "user",      "content": "say pong"}
  ],
  "stream": true,                          // optional
  "session_id": "abc-123",                 // optional ccproxy extension (see X-CC-Session)
  "reasoning_effort": "high"               // optional: low | medium | high | xhigh | max
}
```

`temperature`, `top_p`, `max_tokens`, `tools` are accepted but ignored in v1; surfaced via the `X-CC-Ignored-Params` response header.

### Headers

| Header | Direction | Purpose |
|---|---|---|
| `Authorization: Bearer <token>` | in | Required on `/v1/*`. Token format: `ccp_<8hex>_<24hex>`. |
| `X-CC-Session: <id>` | in | Pin the request to a persistent session. Requires `session:persistent` scope. |
| `X-CC-Workspace: <name>` | in | Target a configured workspace. Requires `workspace:<name>` scope (or `workspace:*`). |
| `X-CC-Verbosity: text-only\|verbose\|narrated` | in | Stream detail level. Falls back to per-token default, then config `default_verbosity`. |
| `X-CC-Effort: low\|medium\|high\|xhigh\|max` | in | Same as request body `reasoning_effort`; header wins. |
| `X-CC-Claude-Model: <name>` | in | Pick the underlying Claude model (`opus\|sonnet\|haiku` or a full id). Overrides per-alias and config defaults. |
| `X-Request-Id` | in/out | Echoed; generated if absent. Threaded into logs. |
| `X-CC-Ignored-Params` | out | Comma list of accepted-but-ignored OpenAI fields. |

### Verbosity modes

- **`text-only`** (default): only assistant text reaches the client. Tool use happens invisibly.
- **`verbose`**: tool calls surface as `delta.tool_calls`; tool results as synthetic `role:"tool"` chunks; `thinking` blocks prefixed with `[thinking]`.
- **`narrated`**: tool calls render as inline `🔧 toolname(args)` lines with `↳ result` follow-ups. Best for human-in-the-loop chat UIs.

### Reasoning effort

Map of OpenAI `reasoning_effort` → `claude --effort`:

| `reasoning_effort` | passed to claude | meaning |
|---|---|---|
| `low` | `--effort low` | cheap, fast |
| `medium` | `--effort medium` | default-ish |
| `high` | `--effort high` | careful |
| `xhigh` | `--effort xhigh` | extra careful (Claude extension) |
| `max` | `--effort max` | maximum (Claude extension) |
| (omitted) | — | leave it to claude's default |

Resolution order (first non-empty wins): `X-CC-Effort` header → request body `reasoning_effort` → model alias `effort:` → top-level `default_effort:` → unset.

### Underlying Claude model

The `model` field accepts three kinds of value, in this lookup order:

1. **A configured alias** (declared under `models:` in `config.yaml`) — gets you a workspace binding, system prompt, and per-alias effort/claude_model defaults. Best for stable client config.
2. **A bare family name**: `opus`, `sonnet`, `haiku`. Resolves through `model_versions:` in config so ops can pin which exact version each name means.
3. **A full Claude id**: `claude-opus-4-7`, `claude-sonnet-4-6`, `claude-haiku-4-5`. Passed straight to `claude --model`.

Example config:

```yaml
allow_passthrough_models: true   # default; set false for strict allowlist mode
default_claude_model: sonnet     # used when an alias doesn't set claude_model
model_versions:
  opus:   claude-opus-4-7
  sonnet: claude-sonnet-4-6
  haiku:  claude-haiku-4-5
models:
  - id: claude-code              # uses default (sonnet)
  - id: claude-code-fast
    claude_model: haiku
  - id: claude-code-deep
    claude_model: opus
```

With this config, all of these requests work:

```jsonc
{"model":"claude-code"}              // alias → sonnet (the default)
{"model":"claude-code-fast"}         // alias → haiku
{"model":"haiku"}                    // passthrough family → claude-haiku-4-5 (pinned)
{"model":"claude-haiku-4-5"}         // passthrough full id → as-is
{"model":"opus"}                     // passthrough family → claude-opus-4-7 (pinned)
```

Override the underlying model at call time with the `X-CC-Claude-Model` header. The header always wins, even when the body sets a `model:` alias.

Final resolution order for the underlying claude model (first non-empty wins): `X-CC-Claude-Model` header → resolved alias's `claude_model:` (or for passthrough, the resolved family/full id) → top-level `default_claude_model:` → unset (claude picks).

`GET /v1/models` lists the configured aliases plus the three family names (when passthrough is enabled), so OpenAI clients with model pickers see all the options. Full version ids aren't enumerated — too many, evolve too fast — but they still work in the request body.

### Workspace (where Claude runs)

Claude Code needs a working directory — where file reads, writes, and shell commands happen. ccproxy resolves one per request using this chain (first hit wins, PRD §6.4):

1. **`X-CC-Workspace: <name>`** request header. Looked up in `config.yaml` under `workspaces:`. Requires the bearer to have `workspace:<name>` (or `workspace:*`) scope.
2. **The alias's `workspace:` field** in `config.yaml`. Static per-alias binding.
3. **Existing persistent-session workspace.** Applied by the session manager when a request reuses an `X-CC-Session` that already has a bound workspace.
4. **Fallback: a fresh ephemeral dir** under `$CCPROXY_STATE/tmp/<uuid>/`, deleted at end-of-request (stateless) or at session TTL (persistent).

Clients **never** supply free-form paths — only names that resolve against server-side config. This is the product's security perimeter.

**Consequence for passthrough models** (`haiku`, `sonnet`, `opus`, full Claude ids): they carry no alias-level workspace binding, so unless the client sends `X-CC-Workspace` they land on step 4 — an ephemeral tmp dir. That's why asking a passthrough model to "create a file" appears to succeed but the file vanishes immediately. Ephemeral scratch space is the point for one-off questions; it's a surprise if you wanted real persistence.

**Three ways to target a real directory:**

**A. Alias with a binding** — best for clients that can be configured once (Chatbox, TypingMind, LibreChat):

```yaml
workspaces:
  myproject:
    path: /home/alice/projects/myproject
models:
  - id: myproject-code
    workspace: myproject
    claude_model: sonnet
```

Client then sends `{"model": "myproject-code"}`.

**B. Per-request header** — most flexible, works with any model including passthrough. Requires the client to support custom headers:

```
POST /v1/chat/completions
Authorization: Bearer ccp_...
X-CC-Workspace: myproject
Content-Type: application/json

{"model": "haiku", "messages": [...]}
```

**C. Persistent session** — first request's workspace (from A or B) sticks to the session id and is reused until TTL:

```
X-CC-Session: my-chat-abc
```

Subsequent requests with the same session id inherit the bound workspace without respecifying it.

### Permission mode

Claude Code defaults to prompting interactively before running tools that modify the filesystem or shell. ccproxy runs it under `-p` with no TTY — so any such prompt hangs the request. The `default_permission_mode:` knob in `config.yaml` controls this, with a sensible default of `bypassPermissions` (ccproxy's threat model already gates access at the **token** layer — if a bearer has `workspace:myproject` scope, the operator already decided to trust it in that directory).

Values:

| Mode | Meaning | Suitable for ccproxy? |
|---|---|---|
| `bypassPermissions` | No prompts, all tools run | yes — the default |
| `dontAsk` | Alias of `bypassPermissions` | yes |
| `acceptEdits` | Auto-accept file edits, still gate Bash/other | partial — Bash calls will hang |
| `default` / `auto` / `plan` | Interactive modes | no — will hang |

Override per-alias with `permission_mode:` on a `models:` entry. Leave `default_permission_mode:` empty if you actually want claude's native behavior (not recommended under this proxy).

### Auth scopes

| Scope | Grants |
|---|---|
| `chat` | `POST /v1/chat/completions`, `GET /v1/models`. Default for new tokens. |
| `session:persistent` | Use `X-CC-Session` / `session_id` for cross-request state. |
| `workspace:<name>` | Target a specific configured workspace. |
| `workspace:*` | Wildcard over all workspaces. |
| `admin` | (reserved for future admin endpoints) |

## Configuration

Resolution order (first match wins):

1. `--config <path>`
2. `$CCPROXY_CONFIG`
3. `$CCPROXY_STATE/config.yaml`
4. `./ccproxy.yaml`
A config file is **required** — startup fails with a clear error if none is found. The minimal valid config is just `allow_passthrough_models: true` (which is the default), letting clients drive ccproxy with bare `model: "haiku"` requests without declaring any aliases.

Env vars are intentionally minimal — behavior lives in YAML. Full reference:

| Variable | Purpose | Default |
|---|---|---|
| `CCPROXY_CONFIG` | Explicit path to `config.yaml`. Overrides the resolver below. | unset |
| `CCPROXY_STATE` | State dir: SQLite token DB, ephemeral workspaces, `captures/`. | `/var/lib/ccproxy` |
| `CCPROXY_LISTEN` | HTTP listener bind address. | `:4141` |
| `CCPROXY_METRICS_LISTEN` | Prometheus `/metrics` listener (separate port, no auth). | unset (disabled) |
| `CCPROXY_TOKEN` | First-boot bearer seed in `ccp_<prefix>_<secret>` format. Ignored after the token DB has at least one row. | unset (auto-mint) |
| `CCPROXY_LOG_FORMAT` | `json` or `text`. Auto-detects from TTY when unset. | auto |

Reference templates: [`deploy/env.example`](deploy/env.example) for systemd `EnvironmentFile=`, [`.env.example`](.env.example) for local dev.

A documented schema sample for `config.yaml` lives at [`deploy/config.yaml.example`](deploy/config.yaml.example).

## Debug capture

Per-token toggle that tees the full request lifecycle to disk for offline inspection. Off by default. When on, every `/v1/chat/completions` request writes a JSONL file at `$CCPROXY_STATE/captures/<request-id>.jsonl` (dir `0700`, file `0600`). Useful for reproducing bad responses, debugging a flaky MCP tool, or diffing verbosity modes against the same prompt.

**Enable the flag.** At creation:

```sh
ccproxy token create --name debug --scopes 'chat,workspace:*' --debug-capture
```

Or on an existing token (takes a token id from `ccproxy token list`, or a unique `--name` value):

```sh
ccproxy token update dev --debug-capture on
# turn it off again:
ccproxy token update dev --debug-capture off
```

`ccproxy token list` shows a `CAPTURE` column so you can see which tokens have it on. `revoke`, `rotate`, and `update` all accept either the token id or the unique name. Toggling invalidates the in-memory auth cache, so the change takes effect on the next request — no restart needed.

**What lands in the file**, one JSONL line per entry:

| `kind` | Payload |
|---|---|
| `request` | `{method, path, query, headers, body}`. `Authorization` is redacted. |
| `event` | Raw `claude.Event` off the subprocess stream — system init, assistant deltas, tool use/result, result. |
| `chunk` | The OpenAI SSE chunk ccproxy emitted (streaming mode only). |
| `response` | The full buffered JSON response (non-streaming mode only). |
| `done` | Terminal marker (streaming mode only). |

**Caveats.** Captures never rotate or prune — add a cron/logrotate rule if you leave this on. The file is opened `O_EXCL` on the request id, so duplicate trace-ids fail to capture (they don't collide in practice — UUIDs). Body size inherits the 4 MiB request cap. Capture failures never fail the request (fail-open on purpose).

## Testing

```sh
make test                                # unit tests
make test-race                           # with race detector
make test-integration                    # real `claude` binary required; //go:build integration
```

The `claude` subprocess and HTTP unit tests use a `TestHelperProcess`-style fake: the test binary re-invokes itself in helper mode (env `CCPROXY_HELPER=1` selects canned NDJSON). No real `claude` is spawned. The integration suite under `tests/integration/` exercises the server in-process via `httptest` against a real `claude` on PATH — covers streaming, non-streaming, persistent session resume, and debug capture. Tests skip cleanly when the binary is missing. Override the per-test deadline with `CCPROXY_INTEGRATION_TIMEOUT` (default 2m).

## Dependencies

| Package | Why |
|---|---|
| `github.com/google/uuid` | Trace IDs, session ids |
| `github.com/joho/godotenv` | `.env` auto-loader |
| `github.com/prometheus/client_golang` | `/metrics` |
| `golang.org/x/crypto/argon2` | Token hashing (PRD §6.6) |
| `golang.org/x/time/rate` | Per-token RPM bucket |
| `gopkg.in/yaml.v3` | Config parser |
| `modernc.org/sqlite` | Token store + daily counter (pure Go, no CGO) |

No web framework. No ORM. Stdlib `net/http` + `database/sql`.

## Where things live

- **Add a new HTTP endpoint:** wire it in `server.Server.Handler()` and register a metrics-instrumented handler.
- **Add a new request field:** add to `openai.ChatRequest`, plumb through `translate.RequestToInvocation`, then `claude.Options`.
- **Add a new config field:** add to `config.Config`, set a default in `Defaults()`, validate in `Validate()`, plumb to whichever consumer needs it.
- **Add a new verbosity mode:** extend `translate.Translator.EventToChunks`. Tests live in `internal/translate/verbosity_test.go`.
- **Change auth surface:** look at `internal/auth/store.go` and `internal/server/middleware.go`.

## License

MIT. See [LICENSE](LICENSE).
