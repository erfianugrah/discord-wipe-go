# AGENTS.md — discord-wipe-go

Context for AI agents working in this repo. Read top-to-bottom before
changing anything — most of it is non-obvious from the code alone. This is
the **live** implementation; the Python [`discord-wipe`](https://github.com/erfianugrah/discord-wipe)
is deprecated and historical.

## What this is

A rolling-retention bulk deleter for **your own** Discord messages. Single
static Go binary (cobra CLI), distroless container, one Compose stack.
Deployed on `servarr` (Unraid, composer-managed) as
`ghcr.io/erfianugrah/discord-wipe-go`. Current version: see `version` in
`cmd/discord-wipe/main.go`.

## Hard safety rules (read these or break things)

- **Self-bot.** Automating a user account violates Discord ToS.
  `DELETE_DELAY` (default `1.0s`) is the **safety floor**, not the target
  pace — real pace is header-driven (`max(DELETE_DELAY, Reset-After /
  Remaining)`, see `bucketPacing` in `internal/discord/client.go`). The
  floor defends against Discord's account-level abuse heuristics, which
  watch overall request *frequency* and are separate from per-route
  buckets. **Do not lower it below 0.3s** without redoing that math.
- **Only-my-messages is a load-bearing property.** The user may be an admin
  in many guilds, which grants permission to delete anyone's messages. The
  tool must never enumerate "all messages in channel X" — only paths
  targeting messages where `author_id == me`. Three layers of defence, each
  covered by a test:
  1. Export phase reads `c<id>/messages.json`, which by definition contains
     only the requester's messages (`TestReadChannelMessagesParsesOurMessagesOnly`).
  2. Live phase queries `messages/search?author_id=<self>&max_id=…` so
     Discord server-side-filters (`TestSearchAlwaysSendsAuthorID`).
  3. A 403 on DELETE is terminal `forbidden` and never retried
     (`TestDeleteForbiddenOn403NeverRetries`).
  Any change adding a code path that produces message IDs to delete MUST
  keep all three intact, and add a test.
- **Token is in `.env`, never committed.** `.gitignore` blocks it. Never
  write a token (even a fake-looking one — Discord's scanner revokes it).
- **No refresh flow.** Discord user tokens are static. On 401 the daemon
  parks (`park("token-rejected", …)`) and waits for a manual rotation via
  `.env` + redeploy. 401 is returned as a typed `*discord.AuthError` and
  **never panicked** — a mid-pass token rotation must park, not crash.
- **`state.Deleted` is NOT garbage-collectable by snowflake age.** The set
  holds IDs we *just deleted* — and we only deleted them because they were
  OLDER than the cutoff, so their snowflakes are old by definition. Any
  "drop IDs older than X" GC re-attempts 100% of them next pass (the
  Python v0.3.0 footgun). `TestStateHasNoGCMethod` fails if a
  `GC`/`Prune`/`Compact`/`Trim` method is added to `State`.

## Architecture

```
cmd/discord-wipe/        cobra CLI, one file per command group
  main.go                root cmd, shared flag globals, version, helpers
  run.go                 `run` daemon: export + live-catchup phases, metrics, park
  search.go              `search` + `purge` + the shared liveCatchup engine
  export.go              `export` (backup) + `leave` + `close-dms`
  meta.go                `verify` `discover` `status` `seed-from-export`
internal/discord/        HTTP client: auth, search, delete, rate-limit pacing, net retry
internal/export/         official Discord data-export reader
internal/state/          durable JSON state (atomic save + .bak fallback, RWMutex)
internal/snowflake/      snowflake <-> time helpers (the retention max_id bound)
```

`run --watch` loops forever. Each pass: export phase (first pass only, then
`export_consumed=true`), then live catch-up across all guilds + DMs. Cutoff
= `now - RETENTION_DAYS`, encoded as a snowflake `max_id`. State persists
deleted IDs so a crash mid-pass re-attempts nothing.

Resilience layers (none touch the delete pipeline; all env-bypassable):
heartbeat file → docker HEALTHCHECK (`status` subcommand, no shell in
distroless); `StateUnwritableError`-style park on FS problems;
restart-burst guard (parks if started >5× in <10min — broken-image guard);
Prometheus `/metrics` on :9090; opt-in `NTFY_URL` park webhook;
connection-level retry with bounded backoff in `client.do`.

## Footgun catalog (every one of these has bitten this project)

- **cobra shared-global defaults (v1.0.0 "deletes everything" bug).**
  Flags on multiple commands were bound to the SAME package globals
  (`retentionDays`, `deleteDelay`, …). Go runs `init()` in **filename
  order**, so `search.go` (purge, `retention-days` default **0**) registered
  after `run.go` (default 14) and clobbered the shared variable to 0; pflag
  does not reset an unset flag during Parse. `run` therefore computed
  `cutoff = now - 0` and deleted every message regardless of age. Fix:
  `resolveFloat`/`resolveBool`/`resolveString` guards at the top of
  `cmdRun.Run` / `cmdPurge.Run` re-derive each shared flag from env / the
  command's own default unless explicitly passed. **If you add a command
  that binds an existing shared global with a different default, add a
  resolve guard** — don't trust the global's start value.
  (`TestRunRetentionNotClobberedByPurgeDefault`.)
- **401 panic with no recover() (fixed).** `SearchMessages`/`DeleteMessage`
  panicked on 401 "caught by main", but nothing recovered it. Now they
  return `*AuthError`; loops propagate it and `run` parks.
- **state.Deleted data race (fixed).** The `/metrics` goroutine read
  `Len()`/`ExportConsumed` while the wipe loop wrote via `Mark()`. `State`
  now has a `sync.RWMutex`; use `Len`/`IsDeleted`/`SetExportConsumed`/
  `IsExportConsumed` for any concurrent access, never the raw map/field.
- **0-byte state.json truncation (Bug12).** A torn write / SIGKILL inside
  the writeback window on Unraid shfs zeroed `state.json` and erased a
  completed wipe. `Save()` does `tmp + fsync + rename` and rotates the prior
  good copy to `.bak`; `load()` falls back to `.bak` when `state.json` is
  missing/empty/corrupt (`TestZeroByteStateFallsBackToBak`).
- **Toolchain drift.** `go.mod` pins `go 1.26.4`. CI `setup-go` tracks the
  go.dev manifest, so it's pinned to `1.26`. The Dockerfile builder stays on
  `golang:1.25-alpine` (Docker Hub has no stable `golang:1.26-alpine` tag
  yet) and relies on `GOTOOLCHAIN=auto` to fetch `1.26.4` at build time —
  this is the intended mechanism, not a workaround. Bump the builder image
  tag only once the matching stable golang image is published.

## Commands (dev box)

```sh
go test ./... -race          # 24 tests; CI runs exactly this
go vet ./...
gofmt -l cmd/ internal/
CGO_ENABLED=0 go build -o /dev/null ./cmd/discord-wipe/

# Token check (in-memory only; never write it to a file)
DISCORD_TOKEN=… go run ./cmd/discord-wipe verify

# Dry-run a pass against the live API (reads only; no deletes; separate state)
DISCORD_TOKEN=… go run ./cmd/discord-wipe run --dry-run \
  --export-dir <export> --state ./state-dryrun/state.json --retention-days 14
```

Dry-runs still hit the live API for reads (auth, guilds, DMs, search) — they
just skip DELETE. `--dry-run` does NOT flip `export_consumed`, and still
calls `Mark()` so catch-up doesn't double-count.

## CI / release

- `ci.yml`: `go vet` + `go test -race` + `CGO_ENABLED=0 build` + docker
  build smoke (`run --help`). Must be green on every PR.
- `release.yml`: `main` push → `:main` + `:sha-<short>`; `v*` tag → also
  `:vX.Y.Z` / `:X.Y` / `:X` / `:latest`. Multi-arch amd64+arm64.
- Bump `version` in `main.go` for behaviour changes; tag `vX.Y.Z` from
  `main`. Add a `BugN`-style regression test for each fixed bug.
- `gofmt`, `go vet` clean before commit.

## Production (servarr)

- Compose stack `discord-wipe`, composer-managed but **NOT git-auto-synced**
  (`git/status` → `remote_url: null`). It is a manually-managed stack:
  `compose.yaml` on disk is updated by hand, deploy is image-pull. So a
  change to `compose.yaml` in this repo does NOT auto-deploy.
- **Deploy / redeploy** (in-container stack path is `/opt/stacks/discord-wipe`,
  because composer runs `docker compose` from inside its own container):
  ```sh
  ssh servarr 'docker exec composer sh -c "cd /opt/stacks/discord-wipe && \
    docker compose pull && docker compose up -d"'
  ```
- **Verify the running build:**
  ```sh
  ssh servarr 'docker inspect discord-wipe \
    --format "{{index .Config.Labels \"org.opencontainers.image.revision\"}}"'
  ssh servarr 'docker logs discord-wipe 2>&1 | grep "pass start" | tail -1'
  # cutoff must be ~RETENTION_DAYS ago, NOT "now".
  ```
- **`.env` is load-bearing and fragile.** `compose.yaml` has `env_file: .env`
  and composer stores no env for this stack, so the on-disk `.env`
  (`DISCORD_TOKEN=…`) is the only token source. A re-clone can delete it,
  after which every `up` 500s with `.env not found` (the running container
  keeps working — its env is baked in at create time). Recover WITHOUT the
  token entering the agent's context by piping it out of the running
  container:
  ```sh
  ssh servarr 'ENV=/mnt/user/composer/stacks/discord-wipe/.env; \
    docker exec discord-wipe printenv DISCORD_TOKEN \
      | { IFS= read -r t; printf "DISCORD_TOKEN=%s\n" "$t"; } > "$ENV"; \
    chmod 600 "$ENV"'
  ```
  (distroless has no `printenv`/shell; if that fails, read it from
  `docker inspect discord-wipe --format '{{json .Config.Env}}'` — but that
  prints the token, so prefer not to.)
- **Data** lives at `/mnt/user/discord-wipe/` (override via
  `DISCORD_WIPE_DATA_DIR`), OUTSIDE the stack dir so it survives re-clones:
  `export/` (RO) + `state/` (RW), owned `99:100` to match the nonroot user.
- **Recover a state-loss without a full re-grind:** `seed-from-export`
  marks every export message older than the cutoff as deleted and sets
  `export_consumed=true` (token-less, no API calls). Stop the container
  first, run it as a one-off against the same mounts, then bring the daemon
  back. Only run it when a prior pass is known to have completed the wipe —
  it does NOT verify the messages are actually gone.

## When to ask vs proceed

- **Proceed:** code changes, doc edits, workflow tweaks, dry-runs, Dockerfile
  changes.
- **Ask first:** any real `run`/`purge` (not `--dry-run`) against
  production, changing the only-my-messages defence-in-depth, lowering
  `DELETE_DELAY` below 0.5s, removing `--watch` or state persistence.
