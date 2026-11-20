package main

// Источник Reddit: решение JS-челленджа + парсинг <shreddit-post>.
// Источник данных. Использует только стандартную библиотеку.

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultUA = "reddit-bridge/0.1 (personal self-hosted RSS reader; contact: local)"

type UpstreamError struct {
	Status int
	Msg    string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.Status, e.Msg)
}

type Post struct {
	ID          string   `json:"ID"`
	Kind        string   `json:"Kind"`
	Title       string   `json:"Title"`
	Author      string   `json:"Author"`
	Permalink   string   `json:"Permalink"`
	ContentHTML string   `json:"ContentHTML"`
	Subreddit   string   `json:"Subreddit"`
	CreatedUTC  float64  `json:"CreatedUTC"`
	Categories  []string `json:"Categories"`
	Score       int      `json:"Score"`
	NumComments int      `json:"NumComments"`
}

func (p Post) GUID() string { return p.Kind + "_" + p.ID }

func (p Post) URL() string {
	if strings.HasPrefix(p.Permalink, "http") {
		return p.Permalink
	}
	return "https://www.reddit.com" + p.Permalink
}

type Params struct {
	Sort, T, Q, F, Format, Frontend, U, Include string
	Score, MinComments, Limit                   int
	TExplicit                                   bool
}

type Target struct {
	Kind string `json:"kind"` // "sub" | "user"
	Name string `json:"name"`
}

type Source interface {
	Fetch(t Target, p Params) ([]Post, error)
	Health() map[string]any
	Metrics() string
}

var validT = map[string]bool{
	"hour": true, "day": true, "week": true,
	"month": true, "year": true, "all": true,
}

var tWindows = map[string]float64{
	"hour": 3600, "day": 86400, "week": 7 * 86400,
	"month": 30 * 86400, "year": 365 * 86400,
}

func normalizeT(t string, explicit bool) string {
	if !explicit {
		return "week"
	}
	if !validT[t] {
		return "week"
	}
	return t
}

func safeName(name string) string {
	return regexp.MustCompile(`[^\w\-]`).ReplaceAllString(name, "")
}

func endpointsFor(t Target, p Params, override []string) []string {
	window := normalizeT(p.T, p.TExplicit)
	if t.Kind == "user" {
		return []string{"/user/" + url.PathEscape(safeName(t.Name)) + "/submitted/"}
	}
	if len(override) > 0 {
		return append([]string{}, override...)
	}
	switch p.Sort {
	case "new":
		return []string{"/new/"}
	case "hot":
		return []string{"/hot/"}
	case "top", "relevance", "comments":
		return []string{"/top/?t=" + window}
	default:
		return []string{"/new/", "/hot/", "/top/?t=" + window}
	}
}

var builtinFrontends = map[string]bool{
	"https://www.reddit.com": true,
	"https://old.reddit.com": true,
	"https://reddit.com":     true,
}

func allowedFrontends() map[string]bool {
	allowed := map[string]bool{}
	for k := range builtinFrontends {
		allowed[k] = true
	}
	for _, item := range strings.Split(os.Getenv("FRONTEND_ALLOWLIST"), ",") {
		item = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(item), "/"))
		if item == "" {
			continue
		}
		u, err := url.Parse(item)
		if err == nil && u.Scheme == "https" && u.Host != "" &&
			(u.Path == "" || u.Path == "/") && u.User == nil && u.RawQuery == "" {
			allowed["https://"+u.Host] = true
		}
	}
	return allowed
}

func resolveFrontend(raw string) string {
	if raw == "" {
		return ""
	}
	v := strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if allowedFrontends()[v] {
		return v
	}
	return ""
}

func postURL(p Post, frontend string) string {
	u := p.URL()
	if frontend != "" {
		return strings.Replace(u, "https://www.reddit.com", frontend, 1)
	}
	return u
}

func postContent(p Post, frontend string) string {
	if frontend != "" {
		return strings.ReplaceAll(p.ContentHTML, "https://www.reddit.com/", frontend+"/")
	}
	return p.ContentHTML
}

var (
	reJscToken = regexp.MustCompile(`name="jsc_token"\s+value="([0-9a-f]+)"`)
	reInput    = regexp.MustCompile(`(?i)<input\b[^>]*>`)
	reAttr     = regexp.MustCompile(`([\w-]+)="([^"]*)"`)
	reScript   = regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`)
	reHexTok   = regexp.MustCompile(`["']([0-9a-f]{16,})["']`)
	reEplusE   = regexp.MustCompile(`\(async\s*(\w+)\s*=>\s*(\w+)\s*\+\s*(\w+)\)\("([0-9a-f]+)"\)`)
	reGeneric  = regexp.MustCompile(`\(async[^)]{0,80}\)\("([0-9a-f]{16,})"\)`)
	rePostBlk  = regexp.MustCompile(`(?s)(<shreddit-post(?:\s[^>]*)?>)(.*?)</shreddit-post>`)
	reTagOpen  = regexp.MustCompile(`<(\w+)`)
)

// solveChallenge разбирает страницу челленджа и возвращает поля GET-запроса.
// Несколько стратегий на случай смены разметки Reddit.
func solveChallenge(body string) (url.Values, string, bool) {
	if !strings.Contains(body, "jsc_token") {
		return nil, "no-challenge", false
	}
	fields := url.Values{}
	for _, tag := range reInput.FindAllString(body, -1) {
		attrs := map[string]string{}
		for _, m := range reAttr.FindAllStringSubmatch(tag, -1) {
			attrs[m[1]] = html.UnescapeString(m[2])
		}
		if strings.EqualFold(attrs["type"], "hidden") && attrs["name"] != "" {
			fields.Set(attrs["name"], attrs["value"])
		}
	}
	if m := reJscToken.FindStringSubmatch(body); m != nil && fields.Get("jsc_token") == "" {
		fields.Set("jsc_token", m[1])
	}
	if fields.Get("jsc_token") == "" {
		return nil, "jsc-token-missing", false
	}

	candidate, variant := "", ""
	if m := reEplusE.FindStringSubmatch(body); m != nil && m[1] == m[2] && m[2] == m[3] {
		candidate, variant = m[4], "e+e"
	}
	if candidate == "" {
		if m := reGeneric.FindStringSubmatch(body); m != nil {
			candidate, variant = m[1], "generic-async"
		}
	}
	if candidate == "" {
		for _, script := range reScript.FindAllStringSubmatch(body, -1) {
			if !strings.Contains(script[1], "solution") {
				continue
			}
			for _, m := range reHexTok.FindAllStringSubmatch(script[1], -1) {
				if len(m[1]) > len(candidate) {
					candidate, variant = m[1], "script-hex"
				}
			}
			if candidate != "" {
				break
			}
		}
	}
	if candidate == "" {
		candidate, variant = fields.Get("jsc_token"), "jsc-token-fallback"
	}
	fields.Set("solution", candidate+candidate)
	if fields.Get("js_challenge") == "" {
		fields.Set("js_challenge", "1")
	}
	if fields.Get("jsc_orig_r") == "" {
		fields.Set("jsc_orig_r", "")
	}
	return fields, variant, true
}

func slotHTML(inner, slot string) string {
	needle := `slot="` + slot + `"`
	i := strings.Index(inner, needle)
	if i < 0 {
		return ""
	}
	start := strings.LastIndex(inner[:i], "<")
	gt := strings.Index(inner[i:], ">")
	if start < 0 || gt < 0 {
		return ""
	}
	gt += i
	m := reTagOpen.FindStringSubmatch(inner[start:gt])
	if m == nil {
		return ""
	}
	name := regexp.QuoteMeta(m[1])
	tokRe := regexp.MustCompile(`</?` + name + `\b`)
	tail := inner[gt+1:]
	depth := 1
	for _, loc := range tokRe.FindAllStringIndex(tail, -1) {
		if strings.HasPrefix(tail[loc[0]:loc[1]], "</") {
			depth--
		} else {
			depth++
		}
		if depth == 0 {
			return tail[:loc[0]]
		}
	}
	return ""
}

func stripText(fragment string) string {
	return strings.Join(strings.Fields(html.UnescapeString(
		regexp.MustCompile(`<[^>]+>`).ReplaceAllString(fragment, " "))), " ")
}

var reTextBodyLink = regexp.MustCompile(`(?is)<a\b[^>]*\bslot="text-body"[^>]*>`)
var reAnchorClose = regexp.MustCompile(`(?i)</a\s*>`)

// unwrapTextBody снимает ссылку-обёртку, которой Reddit накрывает весь текст
// поста. Без этого содержимое фида отображается как одна сплошная ссылка.
// Вложенные полезные ссылки внутри текста сохраняются.
func unwrapTextBody(body string) string {
	s := body
	if m := reTextBodyLink.FindStringIndex(s); m != nil {
		s = s[:m[0]] + s[m[1]:]
	} else {
		t := strings.TrimSpace(s)
		low := strings.ToLower(t)
		if !strings.HasPrefix(low, "<a ") && !strings.HasPrefix(low, "<a>") {
			return body
		}
		gt := strings.Index(t, ">")
		if gt < 0 {
			return body
		}
		s = t[gt+1:]
	}
	closes := reAnchorClose.FindAllStringIndex(s, -1)
	if len(closes) == 0 {
		return body
	}
	last := closes[len(closes)-1]
	return s[:last[0]] + s[last[1]:]
}

func contentHTML(attrs map[string]string, inner string) string {
	ptype, href := attrs["post-type"], attrs["content-href"]
	body := unwrapTextBody(slotHTML(inner, "text-body"))
	var b strings.Builder
	if ptype == "image" && href != "" {
		b.WriteString(`<p><img src="` + html.EscapeString(href) + `" /></p>`)
		b.WriteString(`<p>Изображение</p>`)
	} else if (ptype == "link" || ptype == "multi_media" || ptype == "video" || ptype == "gallery") && href != "" {
		domain := attrs["domain"]
		if domain == "" {
			domain = href
		}
		b.WriteString(`<p>` + html.EscapeString(domain) + `</p>`)
		b.WriteString(`<p><a href="` + html.EscapeString(href) + `">Открыть</a></p>`)
	}
	if strings.TrimSpace(body) != "" {
		b.WriteString(body)
	}
	if perma := attrs["permalink"]; perma != "" {
		b.WriteString(`<p><a href="https://www.reddit.com` +
			html.EscapeString(perma) + `">Читать на Reddit</a></p>`)
	}
	return b.String()
}

func parseTimestamp(v string) float64 {
	if len(v) < 19 {
		return 0
	}
	t, err := time.Parse("2006-01-02T15:04:05", v[:19])
	if err != nil {
		return 0
	}
	return float64(t.UTC().Unix())
}

func parseShredditPosts(page, sub string) []Post {
	var posts []Post
	for _, m := range rePostBlk.FindAllStringSubmatch(page, -1) {
		attrs := map[string]string{}
		for _, a := range reAttr.FindAllStringSubmatch(m[1], -1) {
			attrs[a[1]] = html.UnescapeString(a[2])
		}
		pid := attrs["id"]
		if !strings.HasPrefix(pid, "t3_") {
			continue
		}
		cats := []string{}
		if flair := stripText(slotHTML(m[2], "post-flair")); flair != "" {
			cats = append(cats, flair)
		}
		if regexp.MustCompile(`(?i)\bnsfw\b`).MatchString(m[2]) {
			cats = append(cats, "NSFW")
		}
		if regexp.MustCompile(`(?i)\bspoiler\b`).MatchString(m[2]) {
			cats = append(cats, "Spoiler")
		}
		score, _ := strconv.Atoi(attrs["score"])
		comments, _ := strconv.Atoi(attrs["comment-count"])
		posts = append(posts, Post{
			ID: strings.TrimPrefix(pid, "t3_"), Kind: "t3",
			Title: attrs["post-title"], Author: attrs["author"],
			Permalink: attrs["permalink"], CreatedUTC: parseTimestamp(attrs["created-timestamp"]),
			ContentHTML: contentHTML(attrs, m[2]), Categories: cats,
			Score: score, NumComments: comments,
			Subreddit: attrs["subreddit-name"],
		})
		if posts[len(posts)-1].Subreddit == "" {
			posts[len(posts)-1].Subreddit = sub
		}
	}
	return posts
}

func filterPosts(posts []Post, p Params) []Post {
	out := make([]Post, 0, len(posts))
	var cutoff float64
	if p.TExplicit && p.T != "all" {
		if w, ok := tWindows[p.T]; ok {
			cutoff = float64(time.Now().Unix()) - w
		}
	}
	for _, post := range posts {
		if cutoff > 0 && post.CreatedUTC < cutoff {
			continue
		}
		if p.Score >= 0 && p.MinComments >= 0 {
			if post.Score < p.Score && post.NumComments < p.MinComments {
				continue
			}
		} else if p.Score >= 0 && post.Score < p.Score {
			continue
		} else if p.MinComments >= 0 && post.NumComments < p.MinComments {
			continue
		}
		if p.Q != "" && !strings.Contains(strings.ToLower(post.Title+" "+post.ContentHTML), strings.ToLower(p.Q)) {
			continue
		}
		if p.F != "" {
			ok := false
			for _, c := range post.Categories {
				if strings.Contains(strings.ToLower(c), strings.ToLower(p.F)) {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		out = append(out, post)
	}
	sort.Slice(out, func(i, j int) bool {
		if p.Sort == "top" || p.Sort == "hot" || p.Sort == "relevance" || p.Sort == "comments" {
			return out[i].Score > out[j].Score
		}
		return out[i].CreatedUTC > out[j].CreatedUTC
	})
	if len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out
}

// HTMLSource — боевой источник.
type HTMLSource struct {
	ua          string
	client      *http.Client
	endpoints   []string
	historyPath string
	historyDays int
	maxHistory  int

	mu      sync.Mutex
	history map[string]Post
	started time.Time

	fetchTotal, errorTotal             int
	challengeSolves, challengeFailures int
	challengeVariant                   string
	lastSuccess                        time.Time
	lastError                          string
}

func NewHTMLSource(ua string, timeout time.Duration, endpoints []string, historyPath string) *HTMLSource {
	if ua == "" {
		ua = defaultUA
	}
	if len(endpoints) == 0 {
		endpoints = []string{} // [] = авто-маппинг по sort/t; иначе явный override
	}
	jar, _ := cookiejar.New(nil)
	s := &HTMLSource{
		ua: ua, endpoints: endpoints, historyPath: historyPath,
		historyDays: 7, maxHistory: 5000, history: map[string]Post{}, started: time.Now(),
		client: &http.Client{Jar: jar, Timeout: timeout},
	}
	s.loadHistory()
	return s
}

func (s *HTMLSource) resetJar() {
	jar, _ := cookiejar.New(nil)
	s.client.Jar = jar
}

func (s *HTMLSource) doGet(raw string) (int, string, string) {
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		return 0, raw, ""
	}
	req.Header.Set("User-Agent", s.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, raw, ""
	}
	defer resp.Body.Close()
	buf := make([]byte, 0)
	tmp := make([]byte, 32768)
	for {
		n, e := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if e != nil || len(buf) > 2<<20 {
			break
		}
	}
	final := raw
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return resp.StatusCode, final, string(buf)
}

func (s *HTMLSource) fetchPage(raw string) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		status, final, body := s.doGet(raw)
		if final == "" {
			final = raw
		}
		if strings.Contains(body, "jsc_token") && strings.Contains(body, "solution") {
			fields, variant, ok := solveChallenge(body)
			if ok {
				s.challengeSolves++
				s.challengeVariant = variant
				u, err := url.Parse(final)
				if err == nil {
					q := u.Query()
					for k, v := range fields {
						q.Set(k, v[0])
					}
					u.RawQuery = q.Encode()
					status, final, body = s.doGet(u.String())
				}
			} else {
				s.challengeFailures++
				s.lastError = "challenge: " + variant
			}
		}
		if status != 200 {
			s.lastError = fmt.Sprintf("HTTP %d", status)
			if attempt < 2 {
				s.resetJar()
				continue
			}
			return "", &UpstreamError{status, s.lastError}
		}
		if strings.Contains(body, "jsc_token") && strings.Contains(body, "solution") {
			s.challengeFailures++
			s.lastError = "challenge not solved (" + s.challengeVariant + ")"
			if attempt < 2 {
				s.resetJar()
				continue
			}
			return "", &UpstreamError{403, s.lastError}
		}
		if strings.Contains(body, "Welcome to Reddit") && !strings.Contains(body, "shreddit-post") {
			s.lastError = "login wall"
			return "", &UpstreamError{403, s.lastError}
		}
		s.lastSuccess = time.Now()
		s.lastError = ""
		return body, nil
	}
	return "", &UpstreamError{0, "fetch retries exhausted"}
}

func (s *HTMLSource) loadHistory() {
	if s.historyPath == "" {
		return
	}
	data, err := os.ReadFile(s.historyPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(data, &s.history)
}

func (s *HTMLSource) mergeHistory(found map[string]Post, target Target) {
	now := float64(time.Now().Unix())
	for guid, post := range found {
		s.history[guid] = post
	}
	for guid, post := range s.history {
		if now-post.CreatedUTC > float64(s.historyDays*86400) {
			delete(s.history, guid)
		}
	}
	if len(s.history) > s.maxHistory {
		type item struct {
			key string
			ts  float64
		}
		all := make([]item, 0, len(s.history))
		for k, p := range s.history {
			all = append(all, item{k, p.CreatedUTC})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].ts < all[j].ts })
		for _, x := range all[:len(s.history)-s.maxHistory] {
			delete(s.history, x.key)
		}
	}
	for guid, post := range s.history {
		match := strings.EqualFold(post.Subreddit, target.Name)
		if target.Kind == "user" {
			match = strings.EqualFold(post.Author, target.Name)
		}
		if match {
			if _, ok := found[guid]; !ok {
				found[guid] = post
			}
		}
	}
	if s.historyPath != "" {
		if data, err := json.Marshal(s.history); err == nil {
			_ = os.WriteFile(s.historyPath, data, 0644)
		}
	}
}

func (s *HTMLSource) Fetch(target Target, p Params) ([]Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetchTotal++
	found := map[string]Post{}
	base := "https://www.reddit.com"
	for _, path := range endpointsFor(target, p, s.endpoints) {
		var raw string
		if target.Kind == "user" {
			raw = base + path
		} else {
			raw = fmt.Sprintf("%s/r/%s/%s", base, target.Name, strings.TrimPrefix(path, "/"))
		}
		page, err := s.fetchPage(raw)
		if err != nil {
			s.errorTotal++
			s.lastError = err.Error()
			continue
		}
		for _, post := range parseShredditPosts(page, target.Name) {
			found[post.GUID()] = post
		}
	}
	s.mergeHistory(found, target)
	if len(found) == 0 {
		s.errorTotal++
		return nil, &UpstreamError{0, "ни одного поста не распарсено"}
	}
	list := make([]Post, 0, len(found))
	for _, post := range found {
		list = append(list, post)
	}
	return filterPosts(list, p), nil
}

func (s *HTMLSource) Health() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var age any
	if !s.lastSuccess.IsZero() {
		age = int(time.Since(s.lastSuccess).Seconds())
	}
	return map[string]any{
		"type": "RedditHTMLSource (Go)", "uptime_s": int(time.Since(s.started).Seconds()),
		"endpoints": s.endpoints, "endpoints_auto": len(s.endpoints) == 0,
		"history_posts": len(s.history),
		"fetch_total":   s.fetchTotal, "error_total": s.errorTotal,
		"challenge_solves": s.challengeSolves, "challenge_failures": s.challengeFailures,
		"challenge_variant":  s.challengeVariant,
		"last_success_age_s": age, "last_error": s.lastError,
	}
}

func (s *HTMLSource) Metrics() string {
	h := s.Health()
	return fmt.Sprintf(
		"# TYPE reddit_bridge_source_fetch_total counter\nreddit_bridge_source_fetch_total %d\n"+
			"# TYPE reddit_bridge_source_error_total counter\nreddit_bridge_source_error_total %d\n"+
			"# TYPE reddit_bridge_challenge_solves_total counter\nreddit_bridge_challenge_solves_total %d\n"+
			"# TYPE reddit_bridge_challenge_failures_total counter\nreddit_bridge_challenge_failures_total %d\n"+
			"# TYPE reddit_bridge_history_posts gauge\nreddit_bridge_history_posts %d\n",
		h["fetch_total"], h["error_total"], h["challenge_solves"],
		h["challenge_failures"], h["history_posts"])
}
