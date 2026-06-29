# discord-wipe-go

Rolling-retention bulk deleter for **your own** Discord messages. A Go
rewrite of the original Python [`discord-wipe`](https://github.com/erfianugrah/discord-wipe)
(now deprecated). Runs forever as a tiny static container; every pass it
deletes everything you posted older than `RETENTION_DAYS` (default 14),
sleeps `INTERVAL_HOURS`, repeats. Also ships one-shot subcommands for
targeted wipes, backups, and account cleanup.

> [!WARNING]
> Automating a user account technically violates Discord's ToS. This is a
> self-bot. Pacing is deliberately conservative (see **Rate limiting**).
> Use at your own risk, on your own account.

## What a `run` pass does

1. **Export phase** (first pass only). Reads your official Discord data
   export (`Messages/c<id>/messages.json`) from a read-only bind mount and
   deletes every message older than the retention cutoff, then sets
   `export_consumed=true` in the state file so later passes skip it.
2. **Live catch-up** (every pass). Enumerates current guilds
   (`GET /users/@me/guilds`) + open DMs (`GET /users/@me/channels`) and
   queries `messages/search?author_id=<self>&max_id=<cutoff_snowflake>` on
   each scope, deleting every hit. The cutoff is encoded as a Discord
   snowflake so retention is filtered server-side.

State (`state/state.json`) persists deleted message IDs so crashes,
restarts, and repeat passes never re-attempt the same ID. Writes are atomic
(`tmp + fsync + rename`) with a `.bak` fallback.

## Subcommands

| Command | Purpose |
|---|---|
| `run [--watch]` | Rolling-retention daemon (export phase + live catch-up). |
| `purge --guild/--channel` | One-shot targeted wipe. **Retention defaults to 0 — deletes everything in scope.** |
| `search [--content] [--guild/--channel]` | Preview your messages before purging. |
| `export --output DIR` | Back up your messages to JSON before wiping. |
| `leave [--guild / --inactive N]` | Leave servers (optionally only those inactive > N days). |
| `close-dms [--inactive N]` | Close open DMs. |
| `seed-from-export` | Recovery: mark export messages older than the cutoff as already-deleted (no API calls). |
| `status` | Print a state.json summary (no API calls). |
| `discover` | List live guilds, DMs, and export contents. |
| `verify` | Check that the token works. |

Run `discord-wipe <command> --help` for flags.

## Configuration

Token comes from `DISCORD_TOKEN` (env) or `--token`. All `run` flags are
env-backed:

| Env | Flag | Default | Meaning |
|---|---|---|---|
| `RETENTION_DAYS` | `--retention-days` | `14` | Delete messages older than N days. |
| `INTERVAL_HOURS` | `--interval-hours` | `24` | Hours between passes (with `--watch`). |
| `DELETE_DELAY` | `--delete-delay` | `1.0` | Floor seconds between DELETEs (safety floor — see below). |
| `SEARCH_DELAY` | `--search-delay` | `15.0` | Seconds between search-page fetches. |
| `WATCH` | `--watch` | `false` | Loop forever instead of one pass. |
| `STATE_PATH` | `--state` | `/data/state/state.json` | State file. |
| `EXPORT_DIR` | `--export-dir` | `/data/export/Messages` | Read-only export mount. |
| `NTFY_URL` | `--ntfy-url` | – | Webhook fired on every park event. |
| `METRICS_ENABLED` / `METRICS_BIND` | – | `true` / `0.0.0.0:9090` | Prometheus `/metrics`. |

## Rate limiting

`DELETE_DELAY` is a **safety floor**, not the target pace. The real pace is
header-driven: `max(DELETE_DELAY, X-RateLimit-Reset-After / Remaining)`. The
floor defends against Discord's account-level abuse heuristics, which are
separate from per-route buckets and watch overall request *frequency*.
**Do not lower it below ~0.3s.** Note that deleting messages older than
14 days hits a separate, much stricter Discord rate-limit bucket.

## Deploy

Published as `ghcr.io/erfianugrah/discord-wipe-go` (multi-arch amd64+arm64).
A push to `main` builds `:main` + `:sha-<short>`; a `v*` tag also builds
`:vX.Y.Z` / `:X.Y` / `:X` / `:latest`.

```sh
docker compose pull && docker compose up -d
```

`.env` must contain `DISCORD_TOKEN=...` (it is the only source of the token).
Data lives outside the stack dir (default `/mnt/user/discord-wipe/`): the
read-only `export/` and the read-write `state/`.

## Development

```sh
go test ./... -race        # 24 tests
go vet ./...
gofmt -l cmd/ internal/
CGO_ENABLED=0 go build -o /dev/null ./cmd/discord-wipe/
```

See [`AGENTS.md`](AGENTS.md) for architecture, the safety mandate, the
known footguns, and the operations runbook.
