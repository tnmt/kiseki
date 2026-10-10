# kiseki agent

`kiseki agent -config <file>` runs one collection pass and exits. Scheduling is left to a systemd timer or launchd.

## Configuration

JSON. Unknown fields are an error.

```json
{
  "server": "https://kiseki.example.com",
  "token_file": "/home/user/.config/kiseki/token",
  "spool_dir": "/home/user/.local/state/kiseki",
  "timezone": "Asia/Tokyo",
  "overlap_hours": 48,
  "browser_history": {
    "browsers": ["brave-origin", "chrome"],
    "history_paths": [],
    "exclude_domains": ["example.com"]
  },
  "claude_code": {
    "projects_dir": "",
    "exclude_cwd_prefixes": ["/home/user/src/private-project"]
  }
}
```

- `token_file`: the raw token that `kiseki token new` printed to standard error.
- `timezone`: the `tz` recorded with each record. Defaults to the system zone name. Required where the system has no IANA name (`Local`).
- `overlap_hours`: how far back to resend. Defaults to 48.
- `browsers`: profiles are discovered by reading `Local State` in the default data directory. Supported: `brave-origin` and `chrome` (Linux and macOS).
- `history_paths`: `History` files in non-default locations.
- `exclude_domains`: excludes exact host matches and their subdomains. `example.com` excludes `www.example.com` but not `notexample.com`.
- `claude_code.projects_dir`: defaults to `~/.claude/projects`.
- `claude_code.exclude_cwd_prefixes`: sessions whose working directory is under one of these paths are not sent. Matching is by path component, so `/a/sec` does not match `/a/secret`.
- Only the sections present in the configuration (`browser_history`, `claude_code`) run.

## One run (browser history)

1. Send batches left in the spool, oldest first.
2. Fetch the `browser-history` cursor from the server. If that fails, use local state only.
3. Decide where to resume for each profile.
   - A profile with no local state is read in full.
   - Otherwise, resume from the earlier of the local state and the server cursor, minus `overlap_hours`. Restoring the server from a backup moves its cursor back, so the missing records are resent automatically.
4. Copy `History` together with `-wal`/`-shm` to a temporary directory and read the copy, because the browser locks the original.
5. Drop URLs that are not http or https and excluded domains, and replace URL credentials and sensitive query values (`token`, `code`, `key` and so on) with `REDACTED`. Fragments containing sensitive keys are dropped. All of this happens before anything is sent.
6. Send in batches of 500 records or about 4 MiB. 5xx responses and network errors are retried up to three times with exponential backoff; after that the batch goes to the spool. 4xx responses are not retried.
7. Once a batch is sent or spooled, record the latest timestamp per profile in `spool_dir/state.json`.

## Spool

- `pending/`: waiting to be sent. Resent oldest first on the next run.
- `rejected/`: batches the server refused with a 4xx. Kept for investigation and never resent.

## Record shape

- `record_id`: `<browser>/<profile>:<visits.id>`. Profiles with the same name in different browsers do not collide.
- `payload`: `url`, `title`, `browser`, `profile`, `visit_duration_us`, `transition`, `synced`. `synced` is true for visits synced from another device.

## Claude Code sessions

Reads `*.jsonl` under `~/.claude/projects`, including subagent transcripts (`<session>/subagents/agent-*.jsonl`).

### Lines sent

Only the following lines are sent.

| `type` | Handling |
| --- | --- |
| `user`, `assistant`, `system` | Sent. `record_id` is `<sessionId>:<uuid>`; the time is `timestamp` |
| `ai-title` | Sent. `record_id` is `<sessionId>:ai-title`. Uses the time of the preceding message and is overwritten whenever the title changes |
| `attachment` | Not sent. About 40 MB of the roughly 140 MB measured locally, mostly boilerplate such as `total_tokens_reminder` |
| `file-history-snapshot`, `bridge-session`, `queue-operation`, `mode` and others | Not sent. Metadata without a time or `uuid` |

A line with the same `uuid` can appear up to four times in a file. The content is identical and only metadata such as `origin` or `promptId` differs. The last one wins.

### Incremental reads

Session files are append-only, so reading resumes from a per-file byte offset kept in `spool_dir/claude-code-state.json`.

- A final line without a trailing newline is skipped, since the session may still be writing it. The next run reads it once it is complete.
- If a file is shorter than the recorded offset, it is read again from the start.
- If a file's recorded latest time is newer than the server cursor, the server is assumed to have been restored from a backup, and the file is read from the start. Only lines from the server cursor minus `overlap_hours` onward are resent.
- The offset advances only after a batch is sent or spooled. It does not advance when a batch is rejected.

### Processing before sending

- Base64 image data is replaced with `[image data omitted: N bytes]`.
- Strings longer than 256 KiB are truncated and marked `[truncated N bytes]`.
- A line still larger than 900 KiB becomes a stub (`"truncated": true`) that keeps only `type`, `uuid`, `sessionId`, `timestamp`, `cwd`, `parentUuid` and `isSidechain`, so that the server's 1 MiB limit does not reject the whole batch.
- Secrets in known formats are replaced with `[REDACTED]`:
  - private key blocks
  - Anthropic and OpenAI style API keys
  - GitHub tokens
  - AWS access key IDs
  - Slack tokens
  - JWTs
  - `Authorization: Bearer ...`
  - values assigned to names such as `password`, `secret`, `api_key`, `access_token`
  - values assigned to upper-case environment variable names ending in `_TOKEN`, `_KEY`, `_SECRET` or `_PASSWORD`, such as `FOO_TOKEN=...`
  - product keys of the form `XXXXX-XXXXX-XXXXX-XXXXX-XXXXX`
  - values of JSON object keys with those names

Secret removal is best effort against known formats and does not guarantee that nothing leaks. Exclude projects that must never be sent with `exclude_cwd_prefixes`.

### Local measurements

On 2026-10-10, 143 files from one machine were ingested into a temporary server.

- About 25,000 records in 115 sessions, about 68 MB of payload in total.
- The largest payload was 278 KB. No truncation stubs.
- The first ingest took 45 seconds. A second run sent nothing.

### Verifying redaction with gitleaks

The original files and the payloads after agent processing were scanned with `gitleaks dir --redact` and compared.

| | Findings | Breakdown |
| --- | --- | --- |
| Original `~/.claude/projects` | 253 | dropbox-api-token 140, aws-access-token 74, generic-api-key 38, private-key 1 |
| Processed (first version) | 109 | dropbox-api-token 103, generic-api-key 6 |
| Processed (after adding patterns) | 105 | dropbox-api-token 103, generic-api-key 2 |

Among the generic-api-key findings left by the first version, two kinds were real leaks: a Windows product key (in a system information report) and `CLAUDE_CODE_MESSAGING_TOKEN=...`. Both were added to the redaction patterns.

The remaining 105 findings are judged to be false positives.

- dropbox-api-token, 103: `dropbox: ...;` and `DEFAULT_DROPBOX_SETTINGS: ...`, setting names that appear in application source code.
- generic-api-key, 2: `libsecret-1.so -> ...`, a listing of a library symlink.

These were judged from the surrounding context with the secret part masked by `--redact`. The check only covers formats gitleaks knows; leaks in other formats are not measured.

`.gitleaks.toml` and `.githooks/pre-commit` scan the repository itself. Enable the hook with `git config core.hooksPath .githooks`.
