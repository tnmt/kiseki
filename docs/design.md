# kiseki design

kiseki (軌跡, "trajectory") collects a personal lifelog and stores it on a server away from home.

## Scope

- obsidian-daily-inbox (Inbox below) stays a transient view that owns no data. kiseki holds the lifelog itself.
- Inbox reads kiseki through one additional `ContextSource` that calls the kiseki API. That work belongs to Inbox and is out of scope here.
- Location data is owned by overland-server and is not copied into kiseki.

## Decisions

| Item | Decision |
| --- | --- |
| Language | Go |
| Structure | One repository and one binary, with `kiseki serve` and `kiseki agent` subcommands |
| Server | A NixOS server away from home, managed with a NixOS module |
| Network | Closed inside a VPN, not exposed to the public internet |
| Storage | SQLite (WAL) in three layers: raw, projection and search (below). Raw JSON is stored in plain text |
| Idempotency | `UNIQUE(source, host, record_id)` |
| Backup | Daily restic backups to cloud storage through the rclone backend (below) |
| Authentication | A Bearer token per agent (the same scheme as overland-server) |
| Web frontend | Embedded in `kiseki serve`, read-only, Go `html/template` and htmx |
| Sources | The same set as Inbox (below) |

## Sources

The sources under Inbox's `src/sources/`.

| Source | Handling in kiseki | Collected by |
| --- | --- | --- |
| browser-history | Stored. Chromium deletes history after about 90 days | agent |
| Claude Code sessions | Stored. `~/.claude/projects` | agent |
| withings | Stored. Raw data dumped periodically | serve (collector) |
| github | Stored. Low priority because it can be backfilled | serve (collector) |
| location | Not copied. overland-server owns it | none |
| dropbox | Not stored. The originals live in Dropbox and do not disappear | none |
| on-this-day | Not stored. It is a view derived inside Inbox | none |

Inbox has no Claude Code source yet. The oldest local session file was 28 days old at the time of checking, with no `cleanupPeriodDays` setting. That is consistent with an automatic deletion after 30 days, but the official behaviour has not been confirmed.

## Architecture

```
[machine A] kiseki agent ──┐
[machine B] kiseki agent ──┼─ VPN ─▶ [server] kiseki serve ─▶ SQLite (WAL)
[machine C] kiseki agent ──┘                │
                                            ├─ collector: withings
                                            └─ collector: github
```

- The agent reads data on the machine (browser history, Claude Code sessions) and pushes it to the ingest API.
- serve accepts the ingest API and fetches API sources (Withings, GitHub) itself.
- Inbox's `ContextSource` uses the read API.

## Data model

Three layers. Only the raw layer is authoritative; the projection and search layers can be rebuilt from it.

### Raw layer

The `records` table.

- `source`: source name (`browser-history` and so on)
- `host`: the machine the record came from, or the server name for server-side collectors
- `record_id`: an ID that is stable within the source, defined per source
- `occurred_at`: when the event happened, in UTC epoch milliseconds
- `tz`: the IANA time zone where the event happened
- `local_date`: `occurred_at` converted with `tz`. Stored so the server alone decides which day a record belongs to
- `ingested_at`: when the server received the record
- `payload`: raw JSON as plain TEXT, uncompressed

Upserts on `UNIQUE(source, host, record_id)` make resending safe. Mutable records (such as GitHub pull requests) are replaced with the latest content under the same `record_id`; old versions are not kept.

`record_id` is defined per source:

- Browser history: the pair of a profile identifier and the visit ID
- Claude Code sessions: `sessionId:uuid` per message

Nothing is compressed. While the data is small, being able to read the JSON directly with `sqlite3` matters more. If disk usage becomes a problem, compressing Claude Code sessions alone is the first option.

### Projection layer

Typed tables per source (such as `browser_visits(url, title, domain, visited_at)`). They can be rebuilt from raw data, so changing their schema never damages the raw data.

### Search layer

An FTS5 virtual table over page titles and URLs, Claude Code session text and commit messages.

## Authentication

- Each agent gets its own Bearer token, limited to writing specific sources.
- serve listens only inside the VPN. Machines outside the VPN cannot reach it.

## Web frontend

Embedded in `kiseki serve` and shipped as one binary. Read-only, with no write features (editing domain exclusions, naming places and so on).

Pages:

- Daily timeline: browser history, Claude Code sessions, weight and GitHub activity for one day in time order. Uses the same day boundaries as Inbox, decided by `local_date`.
- Full-text search over the search layer.
- Ingestion status: the last receive time per host and source, to notice a stopped agent.

Pages are rendered on the server with Go `html/template` and htmx. A SPA would require building Node in Nix and weaken the single-binary advantage.

The frontend sits behind a reverse proxy vhost inside the VPN. On top of that network restriction, browsers log in with a read-only token. Details are in `docs/web.md`.

## Collection logic

Inbox is written in TypeScript and kiseki in Go, so no code is shared. What Inbox has learned is ported as a specification.

- Browser history: copy Chromium's `History` together with `-wal`/`-shm` to a temporary directory and read the copy. Timestamps are microseconds since 1601-01-01 UTC. Inbox spawns `sqlite3`; Go can read the file directly with a SQLite driver.
- Withings: use `getmeas`, paging with `more`/`offset` past 1000 groups. The rate limit is one call per 10 minutes (status 601).
- GitHub: the events endpoint only covers the last 30 days and 300 events, so use the search API instead.

## Withings tokens

Access tokens last three hours and refresh tokens one year, and they rotate on every refresh. If serve's collector and Inbox each hold the same token, they may invalidate each other. Whether separate authorizations can coexist has not been confirmed.

The plan is to fetch only from serve and have Inbox read kiseki's archive.

## Backup

restic sends backups to cloud storage through the rclone backend.

- Method: `services.restic.backups`. The rclone configuration and the repository password are provided through a secret manager (sops-nix or similar).
- Consistency: the WAL-mode database file is never copied directly. A consistent snapshot is written first and backed up instead.
- Frequency: daily. The recovery point objective is one day.
- Encryption: restic encrypts the data, so the storage only holds ciphertext.
- litestream is not used. Data is mostly appended and ingestion is idempotent, so second-level recovery is unnecessary.
- Keep a copy of the restic password outside the server. Without it the backup cannot be restored.

### Resending after a restore

Restoring the server from a daily backup loses up to one day of ingested data. The agent therefore does not rely on its local cursor alone: at startup and before sending, it asks the server for the latest `occurred_at` of its `host` and resends from a fixed period before that. Ingestion is idempotent through `UNIQUE(source, host, record_id)`, so duplicates are harmless.

## Privacy

- Domain exclusion and secret removal happen in the agent, before anything reaches the server.
- Encrypt the server's disk.
- Tokens are limited to specific sources.

## Order of work

Preserve what disappears first.

1. The ingest API and the browser history agent
2. Claude Code sessions
3. The Withings collector
4. The GitHub collector
5. The kiseki `ContextSource` in Inbox

## Open questions

- Deployment is described in `docs/deploy.md`. Restoring from a restic backup has not been tested
- The official retention period of Claude Code sessions
- Secret removal from Claude Code sessions: no leaks remain in the formats gitleaks knows. Leaks in other formats and false replacements have not been measured
- Whether kiseki and Inbox can hold separate Withings authorizations
- A visual review of the web frontend and tuning its look with `scripts/demo.sh`
- Whether machines away from home can stay connected to the VPN. While offline, the agent queues batches and resends them later
