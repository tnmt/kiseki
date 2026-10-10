# kiseki API design

Implemented with the standard library `net/http` (`http.ServeMux` method patterns) and modernc.org/sqlite. The layout follows overland-server.

## Authentication

All tokens are sent as `Authorization: Bearer <token>`. Tokens in query parameters are not accepted.

| Kind | Used for | Issued per |
| --- | --- | --- |
| ingest token | `POST /api/v1/ingest`, `GET /api/v1/cursors` | agent |
| read token | the read API and the web interface | reader (Inbox, browser) |

An ingest token is bound to a `host` and a list of sources it may write. The `host` comes from the token, not the request body, so an agent cannot pose as another machine. An ingest token cannot call the read API, and a read token cannot write.

Token definitions are passed as a JSON file, because the standard library can read it and Nix can generate it. Each entry looks like this:

```json
{ "name": "laptop", "host": "laptop", "sources": ["browser-history", "claude-code"], "token_sha256": "<hex>" }
```

Tokens are stored as hashes, so a leaked definition file does not yield a usable token. Comparison uses `crypto/subtle.ConstantTimeCompare`.

Because the definition file holds only hashes, it can live unencrypted in the server's Nix configuration. The raw token stays secret on each agent's machine. Hashes are only safe when tokens are sufficiently random, so tokens are issued with:

```
kiseki token new -name laptop -host laptop -sources browser-history,claude-code
```

This generates a raw token from 32 random bytes, prints it once, and prints the definition entry (with the hash) as JSON.

## Ingest

### `POST /api/v1/ingest`

```json
{
  "source": "browser-history",
  "records": [
    {
      "record_id": "Default:123456",
      "occurred_at": 1760000000000,
      "tz": "Asia/Tokyo",
      "payload": { "url": "https://example.com/", "title": "Example" }
    }
  ]
}
```

- `source` must be a source the token allows.
- `record_id` is a non-empty string of 1 to 256 bytes.
- `occurred_at` is UTC epoch milliseconds.
- `tz` is an IANA time zone name that the server can resolve with `time.LoadLocation` (tzdata is embedded). The server computes `local_date` from `occurred_at` and `tz`.
- `payload` must be valid JSON. Up to 1 MiB per record, 8 MiB per request and 1000 records per request.
- All records are processed in one transaction. If any record fails validation, the whole request is rejected.
- The same `(source, host, record_id)` is upserted with the latest content, so resending is harmless.
- The source's projection and search layers are updated in the same transaction as the raw layer. A source without a projection definition is rejected with `400`.

Response:

```json
{ "inserted": 12, "updated": 3 }
```

| Status | When |
| --- | --- |
| 200 | Success |
| 400 | Invalid JSON, unknown source, unresolvable `tz`, or a `record_id` or `payload` that fails validation |
| 401 | Missing or unknown token |
| 403 | Source not allowed for the token |
| 413 | Size limit exceeded |
| 5xx | Server failure. The agent resends |

### `GET /api/v1/cursors?source=<name>`

Returns the largest `occurred_at` of the source for the calling agent's `host`.

```json
{ "source": "browser-history", "latest_occurred_at": 1760000000000 }
```

`latest_occurred_at` is `null` when there are no records. The agent calls this at startup and before sending, and resends from a fixed period before it. This also fills the gap automatically after the server is restored from a daily backup.

## Agent behaviour

- Batches that fail to send are saved to a spool directory on disk and resent later, so an offline machine does not lose data.
- 5xx responses and network errors are retried with exponential backoff. 4xx responses are not retried; the batch stays in the spool and is logged so the user notices.
- Secret removal and domain exclusion happen in the agent before sending.

## Read API

Authenticated with a read token.

| Endpoint | Returns |
| --- | --- |
| `GET /api/v1/days/{date}` | All records for the given `local_date` across sources, in time order. Filterable by `source` |
| `GET /api/v1/records` | Filtered by `source`, `host`, `from`, `to` and `q` (full-text search), paged with a cursor |
| `GET /api/v1/status` | The last receive time and record count per host and source |

Inbox's `ContextSource` mainly uses `days/{date}`. As with overland-server, responses reflect the day the server assigned to each record based on `local_date`.

## Common

- `GET /healthz` returns `200` without authentication.
- All responses are `application/json`. Errors are `{ "error": "<message>" }`.
- The API version is part of the URL: `/api/v1/`.

## Code layout

```
cmd/kiseki/main.go          subcommand dispatch (serve / agent)
internal/server/            HTTP handlers and authentication
internal/store/             SQLite, schema, migrations
internal/source/<name>/     per-source projection and index updates (serve) and readers (agent)
internal/collector/<name>/  API collectors that run inside serve (withings, github)
internal/agent/             spool, resending, cursor queries
```

Server-side collectors call the store's write functions directly instead of going through HTTP. Their `host` is the server name.

## Open questions

- The cursor format of `GET /api/v1/records`
- The projection schema for each source, starting from browser history
