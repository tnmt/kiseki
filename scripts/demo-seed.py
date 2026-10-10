# Posts synthetic records so the web interface can be reviewed without real data.
# Usage: demo-seed.py <dir with token file> <host:port>
import json, sys, urllib.request, datetime as dt
D, ADDR = sys.argv[1], sys.argv[2]
tok = open(D + "/token").read().strip()
def post(source, records):
    req = urllib.request.Request(f"http://{ADDR}/api/v1/ingest", json.dumps({"source": source, "records": records}).encode(),
                                 {"Authorization": "Bearer " + tok, "Content-Type": "application/json"})
    print(source, urllib.request.urlopen(req).read().decode())
JST = dt.timezone(dt.timedelta(hours=9))
def ms(h, m, s=0, day=10): return int(dt.datetime(2026, 10, day, h, m, s, tzinfo=JST).timestamp() * 1000)
visits = [
 ("https://github.com/tnmt/kiseki", "tnmt/kiseki: Personal lifelog collection and archive", 9, 5),
 ("https://github.com/tnmt/kiseki/pulls", "Pull requests · tnmt/kiseki", 9, 7),
 ("https://github.com/tnmt/kiseki", "tnmt/kiseki: Personal lifelog collection and archive", 9, 40),
 ("https://pkg.go.dev/modernc.org/sqlite", "sqlite package - modernc.org/sqlite - Go Packages", 9, 15),
 ("https://sqlite.org/fts5.html", "SQLite FTS5 Extension", 9, 20),
 ("https://sqlite.org/fts5.html#the_trigram_tokenizer", "SQLite FTS5 Extension: trigram tokenizer", 9, 22),
 ("https://htmx.org/docs/", "</> htmx ~ Documentation", 10, 2),
 ("https://news.ycombinator.com/", "Hacker News", 12, 30),
 ("https://news.ycombinator.com/item?id=1", "Show HN: I built a lifelog archive for myself", 12, 33),
]
post("browser-history", [{"record_id": f"brave/Default:{i}", "occurred_at": ms(h, m), "tz": "Asia/Tokyo",
    "payload": {"url": u, "title": t, "browser": "brave-origin", "profile": "Default"}} for i, (u, t, h, m) in enumerate(visits)])
sid = "0a1b2c3d-4e5f-6789-abcd-ef0123456789"
def line(i, typ, h, m, content, **kw):
    d = {"type": typ, "uuid": f"m{i}", "sessionId": sid, "cwd": "/home/user/src/kiseki", "message": {"role": typ, "content": content}}
    d.update(kw)
    return {"record_id": f"{sid}:m{i}", "occurred_at": ms(h, m, i), "tz": "Asia/Tokyo", "payload": d}
recs = [
 line(1, "user", 13, 0, "I want a read-only web frontend with htmx, plus full-text search"),
 line(2, "assistant", 13, 1, [{"type": "thinking", "thinking": "The FTS5 trigram tokenizer handles substring matches in Japanese too. Queries under three characters fall back to LIKE."},
                              {"type": "text", "text": "Plan:\n- Render with html/template and htmx\n- Search with FTS5 (trigram)\n- Log in with a read-only token"},
                              {"type": "tool_use", "name": "Bash", "input": {"command": "go test ./...", "description": "run all tests"}}]),
 line(3, "user", 13, 2, [{"type": "tool_result", "content": "ok  \tgithub.com/tnmt/kiseki/internal/store\t0.158s\nok  \tgithub.com/tnmt/kiseki/internal/web\t0.259s"}]),
 line(4, "assistant", 13, 3, [{"type": "text", "text": "Tests pass. Next I will measure performance on real data."}]),
 line(5, "assistant", 13, 4, [{"type": "text", "text": "I asked a subagent to check the search results."}], isSidechain=True),
 {"record_id": f"{sid}:ai-title", "occurred_at": ms(13, 4), "tz": "Asia/Tokyo", "payload": {"type": "ai-title", "sessionId": sid, "aiTitle": "Implement the web frontend"}},
]
post("claude-code", recs)
sid2 = "9f8e7d6c-5b4a-3210-fedc-ba9876543210"
post("claude-code", [
 {"record_id": f"{sid2}:n1", "occurred_at": ms(16, 20), "tz": "Asia/Tokyo", "payload": {"type": "user", "uuid": "n1", "sessionId": sid2, "cwd": "/home/user/src/obsidian-daily-inbox", "message": {"content": "Let's design a ContextSource in Inbox that reads the kiseki API"}}},
 {"record_id": f"{sid2}:n2", "occurred_at": ms(16, 21), "tz": "Asia/Tokyo", "payload": {"type": "assistant", "uuid": "n2", "sessionId": sid2, "cwd": "/home/user/src/obsidian-daily-inbox", "message": {"content": [{"type": "text", "text": "I will mirror the location source."}]}}},
 {"record_id": f"{sid2}:ai-title", "occurred_at": ms(16, 21), "tz": "Asia/Tokyo", "payload": {"type": "ai-title", "sessionId": sid2, "aiTitle": "Read kiseki from Inbox"}},
])
# other days so the index has a history
post("browser-history", [{"record_id": f"brave/Default:d{d}", "occurred_at": ms(11, 0, day=d), "tz": "Asia/Tokyo",
    "payload": {"url": f"https://example.com/{d}", "title": f"Notes for day {d}"}} for d in (7, 8, 9)])
