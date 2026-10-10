# kiseki web interface

A read-only interface embedded in `kiseki serve`. Pages are rendered on the server with `html/template`; htmx is used only for incremental search and for lazily loading the visits of a domain.

## Enabling

```
kiseki serve -tokens-file tokens.json -read-token-file read-token.txt -timezone Asia/Tokyo
```

The pages under `/` are enabled only when `-read-token-file` is given. Without it, only the ingest API and `/healthz` run. `-timezone` decides which day is today. The day of each record comes from its `local_date`, computed from the `tz` recorded with it.

## Authentication

- Enter the read token on the login page. Ingest tokens are not accepted.
- On success the server sets a `kiseki_session` cookie. Its value is derived from the read token with HMAC-SHA256 and is not the token itself. The cookie is `HttpOnly` and `SameSite=Strict`, and `Secure` is added on TLS connections or behind a proxy that sends `X-Forwarded-Proto: https`.
- The cookie lasts 30 days. Replacing the read token invalidates every session.
- A wrong token delays the response by 500 ms.
- The post-login redirect is limited to paths on the same site (`//evil.example` and `https://…` fall back to `/`).

It is meant to be combined with network restrictions, such as exposing it only inside a VPN.

## Pages

| Path | Content |
| --- | --- |
| `/` | The last 30 days, with record counts per source |
| `/day/{date}` | One day. Claude Code is grouped by session and the browser by domain |
| `/day/{date}/browser?domain=` | The URLs htmx loads when a domain is expanded. Repeated visits to a URL are counted |
| `/session/{id}` | A Claude Code conversation, 200 messages per page |
| `/search?q=&source=` | Full-text search. Newest first, up to 100 results |
| `/status` | Count, latest event and last receive time per host and source. Warns when nothing has arrived for 36 hours |

### Day page

- Claude Code: title, project name (the last component of the working directory), start and end time, user, reply and subagent message counts, and the beginning of the first prompt.
- Browser: up to 200 domains, most visited first. The rest are shown only as a count.
- Times are shown in the wall clock of the time zone (`tz`) recorded with each record.

### Session page

Shows text, thinking, tool calls and tool results. Tool results and thinking are collapsed. Parts longer than 4,000 characters (1,500 for tool results) are truncated. From a search result, `?m=<uuid>` opens the page that contains the message.

## Search

`internal/index` extracts the searchable text from each record and writes it to the `search` table (FTS5 with the `trigram` tokenizer). Its `rowid` equals `records.id`, and it is updated in the same transaction as ingestion.

| Source | Title | Body |
| --- | --- | --- |
| browser-history | page title | URL |
| claude-code | the `ai-title` title | user and assistant text, tool names and their main arguments (`command`, `description`, `file_path` and so on), cut at 16 KiB |

- Queries of three or more characters use `trigram` substring matching, case-insensitively.
- Shorter queries scan every record with `LIKE`, because `trigram` cannot look up fewer than three characters.
- The query is quoted as a phrase (`"…"`), so operators such as `OR` and `*` have no effect.
- Migration 2 builds the index for records in an existing database.

## Security

- `html/template` escapes all output according to context. A `javascript:` URL in browser history becomes `#ZgotmplZ`. External links open with `rel="noreferrer noopener"`.
- The CSP starts from `default-src 'none'` and allows scripts and styles only from the site's own files. No inline scripts or styles are used. htmx runs with `allowEval` and `includeIndicatorStyles` disabled.
- Responses carry `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` and `Cache-Control: no-store` (five minutes for static files).
- Only session IDs matching `[0-9a-fA-F-]{8,64}` are accepted.

## Bundled files

`internal/web/static/htmx.min.js` is htmx 2.0.11 (0BSD license), taken from jsDelivr `htmx.org@2.0.11/dist/htmx.min.js`, SHA-256 `d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717`. Nothing is fetched from outside at run time.

## Trying it locally

```
nix develop -c scripts/demo.sh
```

Starts a server with synthetic data at `http://127.0.0.1:18090/`. The login token is `demo-read-token`. Temporary files are removed on exit.

## Performance

Measured on 2026-10-10 against real data from one machine (about 46,000 browser visits and 26,000 Claude Code records).

| Page | Size | Time |
| --- | --- | --- |
| The day with the most visits (1,512) | 54 KB | 0.25 s |
| The day with the most Claude Code records | 24 KB | 0.28 s |
| One page of the largest session (2,153 messages) | 128 KB | 0.15 s |
| Search (three or more characters) | 54–61 KB | 0.03–0.11 s |
| Search (one character) | 50 KB | 0.23 s |

## Not implemented

- Rendering for Withings and GitHub (their collectors do not exist yet). Those sources appear only as counts under "Other" on the day page.
- The JSON API for Inbox (`/api/v1/days/{date}` and others).
