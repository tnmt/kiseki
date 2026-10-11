# Withings collector

`kiseki serve` fetches body measurements from the Withings `getmeas` API (scope `user.metrics`) and stores each measure group as it was returned. It runs inside serve, not in an agent, because the refresh token rotates on every refresh and only one process should hold it.

The API behaviour below was observed by Inbox against a real Body Cardio account (obsidian-daily-inbox `docs/architecture.md`); kiseki relies on it without having tested against the real API itself.

## Setup

1. Register a Withings developer application with the redirect URI `http://127.0.0.1:8123/callback`. A development application may use a loopback redirect and is capped at 10 users.
2. Authorize. `kiseki withings auth` listens on the redirect address until one redirect arrives and exchanges the code at once, since it expires 30 seconds after the redirect. The browser must be able to reach `127.0.0.1:8123` on the machine running the command, so either run it on the server through an SSH tunnel:

   ```
   ssh -L 8123:127.0.0.1:8123 server
   sudo -u kiseki kiseki withings auth -dir /var/lib/kiseki/withings \
     -client-id <id> -client-secret-file <file readable by kiseki>
   ```

   or run it locally with `-dir` pointing at a local directory and copy `token.json` to `/var/lib/kiseki/withings/token.json` on the server (owned by `kiseki`, mode 600). Do not run the collector anywhere else with the same token.
3. Enable the collector:

   ```nix
   services.kiseki.withings = {
     enable = true;
     clientId = "…";
     clientSecretFile = config.sops.secrets.withings_client_secret.path;
   };
   ```

Until `token.json` exists, each run logs `not authorized` and does nothing.

## One run

Runs at start and then every `-withings-interval` (default 1h, at least 10m).

1. Load `token.json`. If the access token expires within 5 minutes, refresh it and write the rotated tokens to `token.json` before using them.
2. Decide the `lastupdate` (modification time) to fetch from:
   - No stored `withings` records: everything (`lastupdate` omitted).
   - Otherwise the earlier of the start of the last completed sync and the newest stored measurement, minus `-withings-overlap` (default 48h). A measurement is always modified at or after it was taken, so the newest stored measurement is a safe bound even when the database was restored from a backup older than `state.json`.
3. Call `getmeas` with `category=1` and page with `more`/`offset` (at most 1000 groups per page). Each page is upserted, and `state.json` records the next offset.
4. When `more` is 0, the start time of the sync becomes `synced_until` in `state.json`.

The documented rate limit is one call per 10 minutes per user (status 601). A full history (back to 2016 on the measured account) can need several pages, so a 601 ends the run, and the next run resumes from the recorded offset with the same `lastupdate`. Groups modified while a sync spans several runs are fetched again by the next sync, because `synced_until` is the time the sync started.

A rejected access token (status 401 or 293; assumed, not observed) triggers one refresh and one retry. A rejected refresh token (status 503 from the token endpoint) is logged as requiring `kiseki withings auth` again; `token.json` is kept as it was.

## Files

In `-withings-dir` (`/var/lib/kiseki/withings` with the NixOS module):

| File | Content |
| --- | --- |
| `token.json` | `userid`, access and refresh tokens, `expires_at`. Replaced atomically |
| `state.json` | `userid`, `synced_until` and, while a sync spans runs, `resume` (`lastupdate`, `offset`, `started_at`). Discarded when the token belongs to another user |

## Record shape

- `host`: `-host` (the NixOS module passes `networking.hostName`).
- `record_id`: `grpid`.
- `occurred_at`: `date` in milliseconds.
- `tz`: the group's `timezone`, else the response's `timezone`, else `-timezone`.
- `payload`: the measure group exactly as returned. Values decode as `value * 10^unit`. Observed types: weight 1, fat-free mass 5, fat ratio 6, fat mass 8, heart rate 11, muscle mass 76, hydration 77, bone mass 88, pulse wave velocity 91, vascular age 155.

## Not handled

- Groups deleted in Withings stay in kiseki.
- Rendering on the day page. Withings records appear only as counts under "Other".
- Whether kiseki and Inbox can hold separate authorizations of the same account.
