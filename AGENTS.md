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
     (`TestDeleteForbiddenOn403NeverRetries`). One exception since
     v1.1.0: error code 50083 ("Thread is archived") maps to a distinct
     `archived` status and goes through the unarchive -> delete ->
     re-archive drain (see Architecture). Every other 4xx stays terminal.
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

Per-scope retention overrides (v1.2.0+): `RETENTION_OVERRIDES` (env,
comma-separated) or `--retention-override` (repeatable) entries of the form
`guild:<id>:<days>` / `channel:<id>:<days>` pin one scope's window while
everything else follows `RETENTION_DAYS` (`overrides.go`). Two caveats:
overrides apply to the **live catch-up phase only** - the export phase keys
off message timestamps, not guilds (export channels carry no guild ID), so
an override LONGER than the global window does not protect export messages
(shorter overrides, the normal case, are unaffected). And a malformed entry
is **fatal at startup**, never silently dropped. Tests:
`TestParseRetentionOverrides`, `TestTargetCutoffSF`, `TestResolveSliceEnv`.

Archived threads (v1.1.0+): Discord refuses deletes inside archived
threads (400/403 with code 50083). `liveCatchup` batches those IDs per
search page (`channelID -> []msgID`) and drains them after the page via
`drainArchivedThreads`: unarchive the thread once
(`PATCH /channels/{id}` `archived=false`), delete the pending messages,
re-archive. Threads that cannot be unarchived (locked / no permission)
leave their IDs UNMARKED so a later pass can retry; re-archive failure
only logs a WARN. Client side this is `SetThreadArchived` plus the
`archived` DeleteResult status. Regression tests:
`TestDeleteArchivedThreadCodeIsArchived`,
`TestDrainArchivedThreadsSequence`,
`TestDrainArchivedThreadsCannotUnarchiveLeavesUnmarked`.

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
- **Archived-thread silent skip (Bug13, fixed in v1.1.0).** Deletes in
  archived threads fail with code 50083; pre-v1.1.0 they were counted
  `forbidden` AND Mark()'d, so the no-progress guard declared the scope
  finished while messages still existed. 50083 now maps to a distinct
  `archived` status handled by the drain; only non-50083 4xx stays
  terminal.
- **Any git-sync of the stack checkout git-cleans it.** The stack has
  `auto_sync=true`: a push to `main` auto-syncs the checkout (but does
  NOT recreate the container), and a manual composer `pull`/`up` syncs
  too. Every sync fast-forwards
  `/var/lib/composer/stacks/discord-wipe` to `origin/main` and removes
  untracked files - wiping `.env`, after which the next `up` fails with
  ".env not found" (observed twice on 2026-08-09: after a manual pull,
  and after a docs push triggered the auto-sync). Recreate `.env`
  (recipe below) after EVERY push and always before an `up`.
- **Toolchain drift.** `go.mod` pins `go 1.26.4`. CI `setup-go` tracks the
  go.dev manifest, so it's pinned to `1.26`. The Dockerfile builder stays on
  `golang:1.25-alpine` (Docker Hub has no stable `golang:1.26-alpine` tag
  yet) and relies on `GOTOOLCHAIN=auto` to fetch `1.26.4` at build time —
  this is the intended mechanism, not a workaround. Bump the builder image
  tag only once the matching stable golang image is published.

## Commands (dev box)

```sh
go test ./... -race          # 33 tests; CI runs exactly this
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

- Compose stack `discord-wipe`, composer-managed. Composer now runs on
  the MS-01 router (ssh alias `nixos`); the stack checkout lives at
  `/var/lib/composer/stacks/discord-wipe` on nixos (in-container path
  `/opt/stacks/discord-wipe`). The stack has `auto_sync=true` with NO
  auto-deploy: a push to `main` auto-syncs the checkout (and git-cleans
  it - see the footgun catalog) but never recreates the container.
  Deploy is always a manual `pull` + `up` via the API below.
- **Deploy / redeploy** via the composer API on nixos. The API key stays
  in the local env and is piped over ssh stdin (one curl per pipe - the
  stdin config is consumed by the first curl):
  ```sh
  printf 'header = "X-API-Key: %s"\n' "$COMPOSER_API_KEY" \
    | ssh nixos 'curl -s --config - -X POST \
        "http://localhost:8080/api/v1/stacks/discord-wipe/pull?async=true"'
  # recreate .env here if the pull wiped it (recipe below), then:
  printf 'header = "X-API-Key: %s"\n' "$COMPOSER_API_KEY" \
    | ssh nixos 'curl -s --config - -X POST \
        "http://localhost:8080/api/v1/stacks/discord-wipe/up?async=true"'
  ```
- **Verify the running build:**
  ```sh
  ssh servarr 'docker inspect discord-wipe \
    --format "{{index .Config.Labels \"org.opencontainers.image.revision\"}}"'
  ssh servarr 'docker logs discord-wipe 2>&1 | grep "pass start" | tail -1'
  # cutoff must be ~RETENTION_DAYS ago, NOT "now".
  ```
- **`.env` is load-bearing and fragile.** `compose.yaml` has `env_file: .env`
  and composer stores no env for this stack, so the on-disk `.env` on nixos
  is the only source of `DISCORD_TOKEN` and (since v1.2.0)
  `RETENTION_OVERRIDES`. Composer pull/up ops git-clean it away (and a
  re-clone deletes it too), after which every `up` 500s with `.env not
  found` (the running container keeps working - its env is baked in at
  create time). Recover WITHOUT the token entering the agent's context by
  piping both keys out of the running container on servarr into the file
  on nixos:
  ```sh
  ssh servarr 'docker inspect discord-wipe \
    --format "{{range .Config.Env}}{{println .}}{{end}}" \
    | grep -E "^(DISCORD_TOKEN|RETENTION_OVERRIDES)=" | tr -d "\r"' \
    | ssh nixos 'cat > /var/lib/composer/stacks/discord-wipe/.env; \
      chmod 600 /var/lib/composer/stacks/discord-wipe/.env'
  ```
  (distroless has no `printenv`/shell, so `docker exec ... printenv`
  FAILS - worse, its error text lands on stdout and will be mistaken for
  the token if captured. The `docker inspect` path above never prints the
  token. Verify with `grep -c '^DISCORD_TOKEN=' .env` = 1; the token line
  alone is 85 bytes. If the running container predates the override being
  set, `RETENTION_OVERRIDES` is not in its env and must be re-added
  manually - the value is confidential and lives ONLY in `.env` + the
  container env, never in this public repo.)
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
