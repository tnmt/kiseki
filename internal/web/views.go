package web

import (
	"encoding/json"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tnmt/kiseki/internal/store"
)

const (
	maxDomains      = 200
	maxURLsPerHost  = 300
	messagesPerPage = 200
	searchLimit     = 100
	staleAfter      = 36 * time.Hour
)

var sessionIDPattern = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

func itoa(n int) string { return strconv.Itoa(n) }

// count formats n with a noun that takes a plain "s" plural.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// pastedTag matches the tags Claude Code wraps around pasted text. They are
// noise in an excerpt; the pasted text itself is kept.
var pastedTag = regexp.MustCompile(`</?pasted_content\b[^>]*>`)

func sourceLabel(source string) string {
	switch source {
	case "browser-history":
		return "Browser"
	case "claude-code":
		return "Claude Code"
	case "withings":
		return "Withings"
	case "github":
		return "GitHub"
	}
	return source
}

var locCache = map[string]*time.Location{}

func zone(name string) *time.Location {
	if l, ok := locCache[name]; ok {
		return l
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		l = time.UTC
	}
	return l
}

// clock formats a record's time on the wall clock of the zone it was recorded in.
func clock(ms int64, tz, layout string) string {
	return time.UnixMilli(ms).In(zone(tz)).Format(layout)
}

func parseDate(s string) (time.Time, bool) {
	t, err := time.Parse(time.DateOnly, s)
	return t, err == nil
}

// ---- index ----

type dayRow struct {
	Date    string
	Counts  []sourceCount
	Weekday string
}

type sourceCount struct {
	Label string
	N     int
}

func (w *Web) handleIndex(rw http.ResponseWriter, r *http.Request) {
	days, err := w.cfg.Store.RecentDays(r.Context(), 30)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	var rows []dayRow
	for _, d := range days {
		row := dayRow{Date: d.Date}
		if t, ok := parseDate(d.Date); ok {
			row.Weekday = weekday(t.Weekday())
		}
		for _, src := range sortedKeys(d.BySource) {
			row.Counts = append(row.Counts, sourceCount{sourceLabel(src), d.BySource[src]})
		}
		rows = append(rows, row)
	}
	w.render(rw, r, "index.html", http.StatusOK, map[string]any{
		"Days": rows, "Today": w.cfg.Now().In(w.cfg.Location).Format(time.DateOnly),
	})
}

func weekday(d time.Weekday) string { return d.String()[:3] }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- day ----

type sessionSummary struct {
	ID          string
	Title       string
	Project     string
	Start, End  string
	User, Reply int
	Sub         int
	FirstPrompt string
}

type domainGroup struct {
	Domain string
	Visits int
	Last   string
}

type claudeLine struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	CWD         string `json:"cwd"`
	AITitle     string `json:"aiTitle"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func sessionOf(recordID string) string {
	id, _, _ := strings.Cut(recordID, ":")
	return id
}

func (w *Web) handleDay(rw http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	t, ok := parseDate(date)
	if !ok {
		w.notFound(rw, r)
		return
	}
	rows, err := w.cfg.Store.RecordsForDate(r.Context(), date, "")
	if err != nil {
		w.fail(rw, r, err)
		return
	}

	sessions := map[string]*sessionSummary{}
	var order []string
	domains := map[string]*domainGroup{}
	visits := 0
	other := map[string]int{}
	var withingsRows []store.Row

	for _, row := range rows {
		switch row.Source {
		case "claude-code":
			id := sessionOf(row.RecordID)
			s := sessions[id]
			if s == nil {
				s = &sessionSummary{ID: id, Start: clock(row.OccurredAt, row.TZ, "15:04")}
				sessions[id] = s
				order = append(order, id)
			}
			s.End = clock(row.OccurredAt, row.TZ, "15:04")
			var l claudeLine
			if json.Unmarshal([]byte(row.Payload), &l) != nil {
				continue
			}
			if l.CWD != "" && s.Project == "" {
				s.Project = projectName(l.CWD)
			}
			switch {
			case l.Type == "ai-title":
				s.Title = l.AITitle
			case l.IsSidechain:
				s.Sub++
			case l.Type == "user" && !l.IsMeta:
				if text := plainText(l.Message.Content); text != "" {
					s.User++
					if s.FirstPrompt == "" {
						s.FirstPrompt = truncate(pastedTag.ReplaceAllString(text, " "), 120)
					}
				}
			case l.Type == "assistant":
				s.Reply++
			}
		case "browser-history":
			var p struct {
				URL string `json:"url"`
			}
			if json.Unmarshal([]byte(row.Payload), &p) != nil {
				continue
			}
			host := hostOf(p.URL)
			g := domains[host]
			if g == nil {
				g = &domainGroup{Domain: host}
				domains[host] = g
			}
			g.Visits++
			g.Last = clock(row.OccurredAt, row.TZ, "15:04")
			visits++
		case "withings":
			withingsRows = append(withingsRows, row)
		default:
			other[row.Source]++
		}
	}

	var sessionList []*sessionSummary
	for _, id := range order {
		sessionList = append(sessionList, sessions[id])
	}
	domainList := make([]*domainGroup, 0, len(domains))
	for _, g := range domains {
		domainList = append(domainList, g)
	}
	sort.Slice(domainList, func(i, j int) bool {
		if domainList[i].Visits != domainList[j].Visits {
			return domainList[i].Visits > domainList[j].Visits
		}
		return domainList[i].Domain < domainList[j].Domain
	})
	hidden := 0
	if len(domainList) > maxDomains {
		hidden = len(domainList) - maxDomains
		domainList = domainList[:maxDomains]
	}
	var otherList []sourceCount
	for _, src := range sortedKeys(other) {
		otherList = append(otherList, sourceCount{sourceLabel(src), other[src]})
	}

	w.render(rw, r, "day.html", http.StatusOK, map[string]any{
		"Date": date, "Weekday": weekday(t.Weekday()),
		"Prev": t.AddDate(0, 0, -1).Format(time.DateOnly), "Next": t.AddDate(0, 0, 1).Format(time.DateOnly),
		"WeighIns": weighIns(withingsRows),
		"Sessions": sessionList, "Domains": domainList, "HiddenDomains": hidden, "Visits": visits,
		"Other": otherList, "Empty": len(rows) == 0,
	})
}

type urlGroup struct {
	URL, Title  string
	Count       int
	First, Last string
}

func (w *Web) handleBrowserDomain(rw http.ResponseWriter, r *http.Request) {
	date, domain := r.PathValue("date"), r.URL.Query().Get("domain")
	if _, ok := parseDate(date); !ok {
		http.NotFound(rw, r)
		return
	}
	rows, err := w.cfg.Store.RecordsForDate(r.Context(), date, "browser-history")
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	groups := map[string]*urlGroup{}
	var order []string
	for _, row := range rows {
		var p struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		}
		if json.Unmarshal([]byte(row.Payload), &p) != nil || hostOf(p.URL) != domain {
			continue
		}
		g := groups[p.URL]
		if g == nil {
			g = &urlGroup{URL: p.URL, Title: p.Title, First: clock(row.OccurredAt, row.TZ, "15:04")}
			groups[p.URL] = g
			order = append(order, p.URL)
		}
		g.Count++
		g.Last = clock(row.OccurredAt, row.TZ, "15:04")
		if p.Title != "" {
			g.Title = p.Title
		}
	}
	list := make([]*urlGroup, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		list = append(list, groups[order[i]])
	}
	more := 0
	if len(list) > maxURLsPerHost {
		more = len(list) - maxURLsPerHost
		list = list[:maxURLsPerHost]
	}
	w.renderFragment(rw, "domain_urls", map[string]any{"URLs": list, "More": more})
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "(unknown)"
	}
	return strings.ToLower(u.Hostname())
}

func projectName(cwd string) string {
	if cwd == "" {
		return ""
	}
	return path.Base(cwd)
}

// plainText returns the text of a user message: its string content, or the
// text blocks of an array. Tool results are not text the user wrote.
func plainText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func truncate(s string, runes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= runes {
		return s
	}
	r := []rune(s)
	return string(r[:runes]) + "…"
}

// ---- session ----

type part struct {
	Kind      string // text, tool_use, tool_result, thinking, image
	Text      string
	Name      string
	Truncated bool
}

type message struct {
	UUID      string
	Role      string
	Sidechain bool
	Time      string
	Parts     []part
}

type sessionBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    map[string]any  `json:"input"`
	Content  json.RawMessage `json:"content"`
}

func (w *Web) handleSession(rw http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !sessionIDPattern.MatchString(id) {
		w.notFound(rw, r)
		return
	}
	rows, err := w.cfg.Store.SessionRecords(r.Context(), id)
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	if len(rows) == 0 {
		w.notFound(rw, r)
		return
	}

	var title, project, cwd string
	var all []message
	var first, last store.Row
	for _, row := range rows {
		var l claudeLine
		if json.Unmarshal([]byte(row.Payload), &l) != nil {
			continue
		}
		if first.ID == 0 {
			first = row
		}
		last = row
		if l.CWD != "" && cwd == "" {
			cwd = l.CWD
			project = projectName(l.CWD)
		}
		if l.Type == "ai-title" {
			title = l.AITitle
			continue
		}
		if l.Type != "user" && l.Type != "assistant" {
			continue
		}
		m := message{UUID: l.UUID, Role: l.Type, Sidechain: l.IsSidechain, Time: clock(row.OccurredAt, row.TZ, "01/02 15:04:05")}
		m.Parts = messageParts(l.Message.Content)
		if len(m.Parts) > 0 {
			all = append(all, m)
		}
	}

	page := 1
	if uuid := r.URL.Query().Get("m"); uuid != "" {
		for i, m := range all {
			if m.UUID == uuid {
				page = i/messagesPerPage + 1
				break
			}
		}
	} else if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	pages := (len(all) + messagesPerPage - 1) / messagesPerPage
	if pages == 0 {
		pages = 1
	}
	page = min(page, pages)
	from := (page - 1) * messagesPerPage
	to := min(from+messagesPerPage, len(all))

	if title == "" {
		title = "(untitled session)"
	}
	w.render(rw, r, "session.html", http.StatusOK, map[string]any{
		"ID": id, "Title": title, "Project": project, "CWD": cwd,
		"Start": clock(first.OccurredAt, first.TZ, "2006-01-02 15:04"), "End": clock(last.OccurredAt, last.TZ, "2006-01-02 15:04"),
		"Total": len(all), "Messages": all[from:to], "Page": page, "Pages": pages,
		"PrevPage": page - 1, "NextPage": page + 1, "Date": first.LocalDate,
	})
}

func messageParts(content json.RawMessage) []part {
	var s string
	if json.Unmarshal(content, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []part{limitPart(part{Kind: "text", Text: s}, 4000)}
	}
	var blocks []sessionBlock
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var parts []part
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, limitPart(part{Kind: "text", Text: b.Text}, 4000))
			}
		case "thinking":
			if strings.TrimSpace(b.Thinking) != "" {
				parts = append(parts, limitPart(part{Kind: "thinking", Text: b.Thinking}, 1500))
			}
		case "tool_use":
			parts = append(parts, limitPart(part{Kind: "tool_use", Name: b.Name, Text: toolSummary(b.Input)}, 300))
		case "tool_result":
			parts = append(parts, limitPart(part{Kind: "tool_result", Text: toolResultText(b.Content)}, 1500))
		case "image":
			parts = append(parts, part{Kind: "image", Text: "(image)"})
		}
	}
	return parts
}

func limitPart(p part, runes int) part {
	if utf8.RuneCountInString(p.Text) > runes {
		p.Text = string([]rune(p.Text)[:runes])
		p.Truncated = true
	}
	return p
}

func toolSummary(input map[string]any) string {
	for _, k := range []string{"description", "command", "file_path", "pattern", "query", "url", "prompt"} {
		if s, ok := input[k].(string); ok && s != "" {
			return strings.Join(strings.Fields(s), " ")
		}
	}
	return ""
}

func toolResultText(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ---- search ----

type hit struct {
	Source   string
	When     string
	Date     string
	Title    string
	Snippet  template.HTML
	Link     string
	External bool
}

func (w *Web) handleSearch(rw http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	source := r.URL.Query().Get("source")
	if source != "browser-history" && source != "claude-code" {
		source = ""
	}
	var hits []hit
	if q != "" {
		found, err := w.cfg.Store.Search(r.Context(), q, source, searchLimit)
		if err != nil {
			w.fail(rw, r, err)
			return
		}
		for _, f := range found {
			hits = append(hits, buildHit(f, q))
		}
	}
	data := map[string]any{"Q": q, "Source": source, "Hits": hits, "Limit": searchLimit, "AtLimit": len(hits) == searchLimit}
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Boosted") != "true" {
		w.renderFragment(rw, "results", data)
		return
	}
	w.render(rw, r, "search.html", http.StatusOK, data)
}

func buildHit(f store.SearchHit, q string) hit {
	h := hit{
		Source: sourceLabel(f.Source), Date: f.LocalDate,
		When: clock(f.OccurredAt, f.TZ, "2006-01-02 15:04"),
	}
	switch f.Source {
	case "browser-history":
		var raw struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		}
		json.Unmarshal([]byte(f.Payload), &raw)
		h.Title, h.Link, h.External = raw.Title, raw.URL, true
		if h.Title == "" {
			h.Title = raw.URL
		}
		h.Snippet = snippet(raw.URL, q, 60)
	case "claude-code":
		sid := sessionOf(f.RecordID)
		_, uuid, _ := strings.Cut(f.RecordID, ":")
		h.Link = "/session/" + sid
		if uuid != "ai-title" {
			h.Link += "?m=" + url.QueryEscape(uuid) + "#m-" + uuid
			h.Title = "Message"
		} else {
			h.Title = f.Title
		}
		text := f.Body
		if text == "" {
			text = f.Title
		}
		h.Snippet = snippet(text, q, 60)
	}
	return h
}

// snippet cuts text around the first case-insensitive match and marks it.
// Everything is HTML-escaped before the marks are added.
func snippet(text, q string, context int) template.HTML {
	text = strings.Join(strings.Fields(text), " ")
	tr, qr := []rune(text), []rune(strings.TrimSpace(q))
	fold := func(rs []rune) []rune {
		out := make([]rune, len(rs))
		for i, r := range rs {
			out[i] = unicode.ToLower(r)
		}
		return out
	}
	ft, fq := fold(tr), fold(qr)
	at := -1
	for i := 0; len(fq) > 0 && i+len(fq) <= len(ft); i++ {
		if string(ft[i:i+len(fq)]) == string(fq) {
			at = i
			break
		}
	}
	if at < 0 {
		return template.HTML(html.EscapeString(truncate(text, context*2)))
	}
	start, end := max(at-context, 0), min(at+len(fq)+context, len(tr))
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(html.EscapeString(string(tr[start:at])))
	b.WriteString("<mark>" + html.EscapeString(string(tr[at:at+len(fq)])) + "</mark>")
	b.WriteString(html.EscapeString(string(tr[at+len(fq) : end])))
	if end < len(tr) {
		b.WriteString("…")
	}
	return template.HTML(b.String())
}

// ---- status ----

type statusRow struct {
	Host, Source     string
	Count            int
	LatestEvent      string
	LatestReceived   string
	Stale            bool
	SinceLastReceive string
}

func (w *Web) handleStatus(rw http.ResponseWriter, r *http.Request) {
	rows, err := w.cfg.Store.Status(r.Context())
	if err != nil {
		w.fail(rw, r, err)
		return
	}
	now := w.cfg.Now()
	loc := w.cfg.Location
	var out []statusRow
	for _, s := range rows {
		age := now.Sub(time.UnixMilli(s.LatestIngestedAt))
		out = append(out, statusRow{
			Host: s.Host, Source: sourceLabel(s.Source), Count: s.Count,
			LatestEvent:      time.UnixMilli(s.LatestOccurredAt).In(loc).Format("2006-01-02 15:04"),
			LatestReceived:   time.UnixMilli(s.LatestIngestedAt).In(loc).Format("2006-01-02 15:04"),
			Stale:            age > staleAfter,
			SinceLastReceive: humanAge(age),
		})
	}
	w.render(rw, r, "status.html", http.StatusOK, map[string]any{"Rows": out})
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m ago"
	case d < 48*time.Hour:
		return itoa(int(d.Hours())) + "h ago"
	}
	return itoa(int(d.Hours()/24)) + "d ago"
}
