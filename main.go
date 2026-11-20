package main

// HTTP-слой, кэш и рендер. Веб-морда web/index.html встраивается в бинарник.

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"html"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

type Config struct {
	Port      int
	TTL       time.Duration
	History   string
	UA        string
	Timeout   time.Duration
	Endpoints []string
}

// ---------------------------------------------------------------- кэш ----

type cacheEntry struct {
	body    []byte
	etag    string
	updated time.Time
}

type CacheResult struct {
	body    []byte
	etag    string
	updated time.Time
	stale   bool
	errMsg  string
}

type Cache struct {
	mu            sync.Mutex
	ttl, cooldown time.Duration
	maxEntries    int
	entries       map[string]cacheEntry
	locks         [64]sync.Mutex
	cooldownUntil map[string]time.Time
	lastErr       map[string]string
	lastSuccess   map[string]time.Time
	staleFlags    map[string]bool
	lastSeen      map[string]time.Time
	started       time.Time
	fetchTotal    int
	errorTotal    int
	evictions     int
}

func NewCache(ttl time.Duration, maxEntries ...int) *Cache {
	max := 256
	if len(maxEntries) > 0 && maxEntries[0] > 0 {
		max = maxEntries[0]
	}
	return &Cache{
		ttl: ttl, cooldown: 60 * time.Second, maxEntries: max,
		entries:       map[string]cacheEntry{},
		cooldownUntil: map[string]time.Time{}, lastErr: map[string]string{},
		lastSuccess: map[string]time.Time{}, staleFlags: map[string]bool{},
		lastSeen: map[string]time.Time{},
		started:  time.Now(),
	}
}

func makeETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:])[:16] + `"`
}

func (c *Cache) lockFor(key string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return &c.locks[h.Sum32()%uint32(len(c.locks))]
}

// note ограничивает все map'ы кэша: при превышении maxEntries вытесняет
// ключи, к которым дольше всего не обращались. Вызывается под c.mu.
func (c *Cache) note(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeen[key] = now
	for len(c.lastSeen) > c.maxEntries {
		oldest, oldestTime := "", time.Time{}
		for k, t := range c.lastSeen {
			if oldest == "" || t.Before(oldestTime) {
				oldest, oldestTime = k, t
			}
		}
		if oldest == "" {
			break
		}
		delete(c.lastSeen, oldest)
		delete(c.entries, oldest)
		delete(c.cooldownUntil, oldest)
		delete(c.lastErr, oldest)
		delete(c.lastSuccess, oldest)
		delete(c.staleFlags, oldest)
		c.evictions++
	}
}

func ptrEntry(has bool, e cacheEntry) *cacheEntry {
	if !has {
		return nil
	}
	return &e
}

func (c *Cache) serveStale(key string, entry *cacheEntry, now time.Time, empty func() []byte) CacheResult {
	c.mu.Lock()
	c.staleFlags[key] = true
	err := c.lastErr[key]
	c.mu.Unlock()
	if entry != nil {
		return CacheResult{entry.body, entry.etag, entry.updated, true, err}
	}
	body := empty()
	return CacheResult{body, makeETag(body), now, true, err}
}

func (c *Cache) Get(key string, produce func() []byte, empty func() []byte) CacheResult {
	now := time.Now()
	c.note(key, now)
	c.mu.Lock()
	entry, has := c.entries[key]
	c.mu.Unlock()
	if has && now.Sub(entry.updated) < c.ttl {
		return CacheResult{entry.body, entry.etag, entry.updated, false, ""}
	}
	c.mu.Lock()
	_, cooling := c.cooldownUntil[key]
	cooling = cooling && now.Before(c.cooldownUntil[key])
	c.mu.Unlock()
	if cooling {
		return c.serveStale(key, ptrEntry(has, entry), now, empty)
	}

	l := c.lockFor(key)
	l.Lock()
	defer l.Unlock()
	now = time.Now()
	c.mu.Lock()
	entry, has = c.entries[key]
	c.mu.Unlock()
	if has && now.Sub(entry.updated) < c.ttl {
		return CacheResult{entry.body, entry.etag, entry.updated, false, ""}
	}
	c.mu.Lock()
	cooling = now.Before(c.cooldownUntil[key])
	c.mu.Unlock()
	if cooling {
		return c.serveStale(key, ptrEntry(has, entry), now, empty)
	}
	c.mu.Lock()
	c.fetchTotal++
	c.mu.Unlock()
	var body []byte
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		body = produce()
	}()
	if err != nil {
		c.mu.Lock()
		c.errorTotal++
		c.lastErr[key] = err.Error()
		c.cooldownUntil[key] = now.Add(c.cooldown)
		c.mu.Unlock()
		return c.serveStale(key, ptrEntry(has, entry), now, empty)
	}
	etag := makeETag(body)
	c.mu.Lock()
	c.entries[key] = cacheEntry{body, etag, now}
	c.lastSuccess[key] = now
	delete(c.lastErr, key)
	delete(c.staleFlags, key)
	delete(c.cooldownUntil, key)
	c.mu.Unlock()
	return CacheResult{body, etag, now, false, ""}
}

func (c *Cache) Health() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var latest time.Time
	for _, t := range c.lastSuccess {
		if t.After(latest) {
			latest = t
		}
	}
	var age any
	if !latest.IsZero() {
		age = int(time.Since(latest).Seconds())
	}
	stale := false
	for _, v := range c.staleFlags {
		if v {
			stale = true
		}
	}
	var lastErr any
	for _, v := range c.lastErr {
		if v != "" {
			lastErr = v
			break
		}
	}
	return map[string]any{
		"uptime_s": int(time.Since(c.started).Seconds()),
		"entries":  len(c.entries), "fetch_total": c.fetchTotal,
		"error_total": c.errorTotal, "stale": stale,
		"max_entries": c.maxEntries, "evictions_total": c.evictions,
		"last_success_age_s": age, "last_error": lastErr,
	}
}

func (c *Cache) Metrics() string {
	h := c.Health()
	out := "# HELP reddit_bridge_up Service is responding.\n# TYPE reddit_bridge_up gauge\nreddit_bridge_up 1\n"
	out += fmt.Sprintf("# TYPE reddit_bridge_cache_entries gauge\nreddit_bridge_cache_entries %d\n", h["entries"])
	out += fmt.Sprintf("# TYPE reddit_bridge_fetch_total counter\nreddit_bridge_fetch_total %d\n", h["fetch_total"])
	out += fmt.Sprintf("# TYPE reddit_bridge_error_total counter\nreddit_bridge_error_total %d\n", h["error_total"])
	out += fmt.Sprintf("# TYPE reddit_bridge_stale gauge\nreddit_bridge_stale %d\n", boolToInt(h["stale"].(bool)))
	out += fmt.Sprintf("# TYPE reddit_bridge_cache_max_entries gauge\nreddit_bridge_cache_max_entries %d\n", h["max_entries"])
	out += fmt.Sprintf("# TYPE reddit_bridge_cache_evictions_total counter\nreddit_bridge_cache_evictions_total %d\n", h["evictions_total"])
	if age, ok := h["last_success_age_s"].(int); ok {
		out += fmt.Sprintf("# TYPE reddit_bridge_last_success_age_seconds gauge\nreddit_bridge_last_success_age_seconds %d\n", age)
	}
	return out
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ------------------------------------------------------------- рендер ----

func iso(ts float64) string {
	if ts <= 0 {
		ts = float64(time.Now().Unix())
	}
	return time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
}

func renderFeed(posts []Post, p Params, subs []string, feedURL string, stale bool) []byte {
	if p.Format == "rss" {
		return renderRSS(posts, p, subs, feedURL, stale)
	}
	return renderAtom(posts, p, subs, feedURL, stale)
}

func feedTitle(p Params, subs []string) string {
	if p.U != "" {
		return "Reddit u/" + p.U
	}
	return "Reddit r/" + strings.Join(subs, ", ")
}

func renderAtom(posts []Post, p Params, subs []string, feedURL string, stale bool) []byte {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n")
	b.WriteString("<feed xmlns=\"http://www.w3.org/2005/Atom\">\n")
	b.WriteString("<title>" + html.EscapeString(feedTitle(p, subs)) + "</title>\n")
	b.WriteString("<id>tag:reddit-bridge,2026:" + html.EscapeString(
		firstNonEmpty(p.U, strings.Join(subs, ","))) + "</id>\n")
	b.WriteString(`<link rel="self" href="` + html.EscapeString(feedURL) + `" />` + "\n")
	latest := 0.0
	for _, post := range posts {
		if post.CreatedUTC > latest {
			latest = post.CreatedUTC
		}
	}
	b.WriteString("<updated>" + iso(latest) + "</updated>\n")
	b.WriteString("<generator>reddit-bridge/0.2-go</generator>\n")
	if stale {
		b.WriteString("<subtitle>upstream unavailable — serving cached feed</subtitle>\n")
	}
	for _, post := range posts {
		b.WriteString("<entry>\n")
		b.WriteString("<id>tag:reddit.com,2026:" + html.EscapeString(post.GUID()) + "</id>\n")
		b.WriteString("<title>" + html.EscapeString(post.Title) + "</title>\n")
		b.WriteString("<updated>" + iso(post.CreatedUTC) + "</updated>\n")
		b.WriteString("<published>" + iso(post.CreatedUTC) + "</published>\n")
		author := post.Author
		if author == "" {
			author = "[deleted]"
		}
		b.WriteString("<author><name>" + html.EscapeString(author) + "</name></author>\n")
		b.WriteString(`<link href="` + html.EscapeString(postURL(post, p.Frontend)) + `" />` + "\n")
		for _, c := range post.Categories {
			b.WriteString(`<category term="` + html.EscapeString(c) + `" />` + "\n")
		}
		b.WriteString(`<content type="html">` + html.EscapeString(postContent(post, p.Frontend)) + "</content>\n")
		b.WriteString("</entry>\n")
	}
	b.WriteString("</feed>\n")
	return []byte(b.String())
}

func renderRSS(posts []Post, p Params, subs []string, feedURL string, stale bool) []byte {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<rss version=\"2.0\"><channel>\n")
	b.WriteString("<title>" + html.EscapeString(feedTitle(p, subs)) + "</title>\n")
	b.WriteString("<link>" + html.EscapeString(feedURL) + "</link>\n")
	b.WriteString("<description>reddit-bridge</description>\n")
	if stale {
		b.WriteString("<description>upstream unavailable — serving cached feed</description>\n")
	}
	for _, post := range posts {
		b.WriteString("<item>\n")
		b.WriteString("<title>" + html.EscapeString(post.Title) + "</title>\n")
		b.WriteString("<link>" + html.EscapeString(postURL(post, p.Frontend)) + "</link>\n")
		b.WriteString(`<guid isPermaLink="false">` + html.EscapeString(post.GUID()) + "</guid>\n")
		b.WriteString("<pubDate>" + iso(post.CreatedUTC) + "</pubDate>\n")
		b.WriteString("<description>" + html.EscapeString(postContent(post, p.Frontend)) + "</description>\n")
		for _, c := range post.Categories {
			b.WriteString("<category>" + html.EscapeString(c) + "</category>\n")
		}
		b.WriteString("</item>\n")
	}
	b.WriteString("</channel></rss>\n")
	return []byte(b.String())
}

// ------------------------------------------------------------ параметры ----

func parseParams(q url.Values) (Params, []string) {
	get := func(k, def string) string {
		if v := q.Get(k); v != "" {
			return v
		}
		return def
	}
	num := func(k string, def int) int {
		if v, err := strconv.Atoi(q.Get(k)); err == nil {
			return v
		}
		return def
	}
	raw := get("r", "selfhosted")
	subs := []string{}
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			subs = append(subs, s)
		}
	}
	if len(subs) == 0 {
		subs = []string{"selfhosted"}
	}
	limit := num("limit", 25)
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	return Params{
		Sort: get("sort", ""), T: get("t", "week"),
		TExplicit: q.Has("t"),
		Q:         get("q", ""), F: get("f", ""), Format: get("format", "atom"),
		Frontend: resolveFrontend(get("frontend", "")),
		U:        get("u", ""), Include: get("include", "posts"),
		Score: num("score", -1), MinComments: num("min_comments", -1),
		Limit: limit,
	}, subs
}

func buildTargets(p Params, subs []string) []Target {
	if p.U != "" {
		return []Target{{Kind: "user", Name: p.U}}
	}
	out := make([]Target, 0, len(subs))
	for _, s := range subs {
		out = append(out, Target{Kind: "sub", Name: s})
	}
	return out
}

func detectRedditURL(raw string) map[string]any {
	if !strings.Contains(raw, "://") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	host := strings.ToLower(u.Hostname())
	if host != "reddit.com" && !strings.HasSuffix(host, ".reddit.com") {
		return map[string]any{"ok": false, "error": "не похоже на reddit.com"}
	}
	parts := []string{}
	for _, s := range strings.Split(u.Path, "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) >= 2 && parts[0] == "r" {
		return map[string]any{"ok": true, "context": "single",
			"params": map[string]string{"r": parts[1]}, "feed": "/feed?r=" + parts[1]}
	}
	if len(parts) >= 2 && (parts[0] == "user" || parts[0] == "u") {
		return map[string]any{"ok": true, "context": "user",
			"params": map[string]string{"u": parts[1]}, "feed": "/feed?u=" + parts[1]}
	}
	return map[string]any{"ok": false, "error": "нужен путь /r/<sub> или /user/<name>"}
}

// --------------------------------------------------------------- сервер ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func handle(cache *Cache, src Source, cfg Config, webPage []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			writeJSON(w, map[string]any{
				"status": "ok", "cache": cache.Health(), "source": src.Health(),
			})
			return
		case "/metrics":
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			fmt.Fprint(w, cache.Metrics()+src.Metrics())
			return
		case "/api/detect":
			writeJSON(w, detectRedditURL(r.URL.Query().Get("url")))
			return
		case "/", "/index.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(webPage)
			return
		case "/feed":
			serveFeed(w, r, cache, src, cfg)
			return
		default:
			http.NotFound(w, r)
		}
	}
}

func serveFeed(w http.ResponseWriter, r *http.Request, cache *Cache, src Source, cfg Config) {
	p, subs := parseParams(r.URL.Query())
	targets := buildTargets(p, subs)
	keyBytes, _ := json.Marshal(struct {
		P       Params   `json:"p"`
		Subs    []string `json:"subs"`
		Targets []Target `json:"targets"`
	}{p, subs, targets})
	key := string(keyBytes)
	feedURL := r.URL.Path
	if r.URL.RawQuery != "" {
		feedURL += "?" + r.URL.RawQuery
	}

	render := func(posts []Post, stale bool) []byte {
		return renderFeed(posts, p, subs, feedURL, stale)
	}
	produce := func() []byte {
		var posts []Post
		for _, target := range targets {
			got, err := src.Fetch(target, p)
			if err != nil {
				panic(err) // кэш превратит это в stale, а не в 5xx
			}
			posts = append(posts, got...)
		}
		sort.Slice(posts, func(i, j int) bool {
			return posts[i].CreatedUTC > posts[j].CreatedUTC
		})
		if len(posts) > p.Limit {
			posts = posts[:p.Limit]
		}
		return render(posts, false)
	}
	res := cache.Get(key, produce, func() []byte { return render(nil, true) })

	ctype := "application/atom+xml"
	if p.Format == "rss" {
		ctype = "application/rss+xml"
	}
	h := w.Header()
	h.Set("Content-Type", ctype+"; charset=utf-8")
	h.Set("ETag", res.etag)
	h.Set("Last-Modified", res.updated.UTC().Format(http.TimeFormat))
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(cfg.TTL.Seconds())))
	if age := cache.Health()["last_success_age_s"]; age != nil {
		h.Set("X-Bridge-Last-Success-Age", fmt.Sprint(age))
	}
	if res.stale {
		h.Set("X-Bridge-Stale", "1")
		h.Set("Warning", `110 - "Response is stale"`)
		if res.errMsg != "" {
			clean := strings.ReplaceAll(res.errMsg, "\n", " ")
			h.Set("X-Bridge-Error", clean[:min(120, len(clean))])
		}
	}
	if p.Include != "posts" {
		h.Set("X-Bridge-Notice", "include=comments/both is phase 2; serving posts only")
	}
	if r.Header.Get("If-None-Match") == res.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != "HEAD" {
		_, _ = w.Write(res.body)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	var port, ttlSec, timeoutSec, cacheMax int
	var history, ua, endpoints string
	flag.IntVar(&port, "port", 8788, "порт")
	flag.IntVar(&ttlSec, "ttl", 1800, "TTL кэша, сек")
	flag.IntVar(&timeoutSec, "timeout", 12, "таймаут HTTP, сек")
	flag.IntVar(&cacheMax, "cache-max", 256, "максимум ключей кэша")
	flag.StringVar(&history, "history", "", "файл истории постов")
	flag.StringVar(&ua, "ua", "", "User-Agent")
	flag.StringVar(&endpoints, "endpoints", "", "ленты через запятую")
	flag.Parse()

	var eps []string
	if endpoints != "" {
		for _, e := range strings.Split(endpoints, ",") {
			if e = strings.TrimSpace(e); e != "" {
				eps = append(eps, e)
			}
		}
	}
	cfg := Config{
		Port: port, TTL: time.Duration(ttlSec) * time.Second,
		History: history, UA: ua, Timeout: time.Duration(timeoutSec) * time.Second,
		Endpoints: eps,
	}
	src := NewHTMLSource(cfg.UA, cfg.Timeout, cfg.Endpoints, cfg.History)
	cache := NewCache(cfg.TTL, cacheMax)
	webPage, err := webFS.ReadFile("web/index.html")
	if err != nil {
		webPage = []byte("<!doctype html><h1>reddit-bridge</h1><a href='/feed?r=selfhosted'>feed</a>")
	}
	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	log.Printf("reddit-bridge (Go) на %s", addr)
	srv := &http.Server{Addr: addr, Handler: handle(cache, src, cfg, webPage),
		ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
