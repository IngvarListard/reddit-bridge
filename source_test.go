package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSolveChallengeEPlusE(t *testing.T) {
	page := `<form><input type="hidden" name="solution"/>
	<input type="hidden" name="js_challenge" value="1"/>
	<input type="hidden" name="jsc_token" value="abc123"/>
	<input type="hidden" name="jsc_orig_r" value=""/></form>
	<script>var n=(async e=>e+e)("4a40e29952fe6e27");
	document.forms[0].solution.value=n;</script>`
	fields, variant, ok := solveChallenge(page)
	if !ok {
		t.Fatal("челлендж не решён")
	}
	if variant != "e+e" {
		t.Fatalf("variant = %q, хотим e+e", variant)
	}
	if got := fields.Get("solution"); got != "4a40e29952fe6e274a40e29952fe6e27" {
		t.Fatalf("solution = %q", got)
	}
	if fields.Get("jsc_token") != "abc123" || fields.Get("js_challenge") != "1" {
		t.Fatal("hidden-поля потеряны")
	}
}

func TestSolveChallengeScriptHex(t *testing.T) {
	page := `<form><input name="jsc_token" value="ffeeddccbbaa9988" type="hidden"/></form>
	<script>var n="0123456789abcdef"; document.forms[0].solution.value=n;</script>`
	fields, variant, ok := solveChallenge(page)
	if !ok || variant != "script-hex" {
		t.Fatalf("ok=%v variant=%q", ok, variant)
	}
	if got := fields.Get("solution"); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("solution = %q", got)
	}
}

func TestSolveChallengeNoChallenge(t *testing.T) {
	_, _, ok := solveChallenge("<html>обычная страница</html>")
	if ok {
		t.Fatal("ложное срабатывание")
	}
}

func TestParseShredditPosts(t *testing.T) {
	data, err := os.ReadFile("fixtures/shreddit_sample.html")
	if err != nil {
		t.Skip("фикстура недоступна:", err)
	}
	posts := parseShredditPosts(string(data), "selfhosted")
	if len(posts) != 3 {
		t.Fatalf("постов = %d, хотим 3", len(posts))
	}
	p := posts[0]
	if p.GUID() != "t3_1y0yhln" || p.Author != "sakuramoto" {
		t.Fatalf("первый пост: %+v", p)
	}
	if p.CreatedUTC <= 0 || p.ContentHTML == "" {
		t.Fatal("пустые дата/контент")
	}
	if len(p.Categories) == 0 {
		t.Fatal("flair не распарсен")
	}
}

func TestDetectRedditURL(t *testing.T) {
	r := detectRedditURL("https://www.reddit.com/r/golang/")
	if r["ok"] != true {
		t.Fatalf("detect: %v", r)
	}
	params := r["params"].(map[string]string)
	if params["r"] != "golang" {
		t.Fatalf("params: %v", params)
	}
	if detectRedditURL("https://example.com/r/x/")["ok"] != false {
		t.Fatal("чужой домен принят")
	}
}

func TestEndpointsFor(t *testing.T) {
	sub := Target{Kind: "sub", Name: "selfhosted"}
	base := Params{T: "week", Limit: 25}
	got := endpointsFor(sub, base, nil)
	if len(got) != 3 || got[2] != "/top/?t=week" {
		t.Fatalf("default: %v", got)
	}
	if got := endpointsFor(sub, Params{Sort: "top", T: "hour", TExplicit: true}, nil); got[0] != "/top/?t=hour" {
		t.Fatalf("top/hour: %v", got)
	}
	if got := endpointsFor(sub, Params{Sort: "new"}, nil); got[0] != "/new/" {
		t.Fatalf("new: %v", got)
	}
	if got := endpointsFor(sub, Params{Sort: "top", T: "мусор", TExplicit: true}, nil); got[0] != "/top/?t=week" {
		t.Fatalf("invalid t: %v", got)
	}
	if got := endpointsFor(Target{Kind: "user", Name: "bob"}, base, nil); got[0] != "/user/bob/submitted/" {
		t.Fatalf("user: %v", got)
	}
	if got := endpointsFor(sub, base, []string{"/new/"}); len(got) != 1 || got[0] != "/new/" {
		t.Fatalf("override: %v", got)
	}
}

func TestResolveFrontend(t *testing.T) {
	if got := resolveFrontend("https://old.reddit.com"); got != "https://old.reddit.com" {
		t.Fatalf("builtin: %q", got)
	}
	if got := resolveFrontend("http://evil.example.com"); got != "" {
		t.Fatalf("http accepted: %q", got)
	}
	if got := resolveFrontend("https://evil.example.com"); got != "" {
		t.Fatalf("unknown host accepted: %q", got)
	}
	t.Setenv("FRONTEND_ALLOWLIST", "https://redlib.example.com, http://bad.example.com")
	if got := resolveFrontend("https://redlib.example.com"); got != "https://redlib.example.com" {
		t.Fatalf("env allowlist: %q", got)
	}
	if got := resolveFrontend("http://bad.example.com"); got != "" {
		t.Fatalf("env http accepted: %q", got)
	}
}

func TestBuildTargets(t *testing.T) {
	p := Params{U: "bob", T: "week"}
	if got := buildTargets(p, []string{"selfhosted"}); len(got) != 1 || got[0].Kind != "user" {
		t.Fatalf("user: %v", got)
	}
	if got := buildTargets(Params{T: "week"}, []string{"a", "b"}); len(got) != 2 || got[1].Name != "b" {
		t.Fatalf("subs: %v", got)
	}
}

func TestAgeFilterOnlyWhenExplicit(t *testing.T) {
	now := float64(time.Now().Unix())
	posts := []Post{
		{ID: "1", Title: "свежий", CreatedUTC: now - 60},
		{ID: "2", Title: "старый", CreatedUTC: now - 10*86400},
	}
	if got := filterPosts(posts, Params{T: "week", Limit: 25}); len(got) != 2 {
		t.Fatalf("дефолт не должен резать: %d", len(got))
	}
	got := filterPosts(posts, Params{T: "day", TExplicit: true, Limit: 25})
	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("day: %v", got)
	}
	if got := filterPosts(posts, Params{T: "all", TExplicit: true, Limit: 25}); len(got) != 2 {
		t.Fatalf("all не должен резать: %d", len(got))
	}
}

func TestFrontendRewritesLinks(t *testing.T) {
	p := Post{ID: "x", Permalink: "/r/selfhosted/comments/x/y/",
		ContentHTML: `<p><a href="https://www.reddit.com/r/selfhosted/">читать</a></p>`}
	if got := postURL(p, "https://old.reddit.com"); !strings.Contains(got, "https://old.reddit.com/r/") {
		t.Fatalf("postURL: %q", got)
	}
	if got := postContent(p, "https://old.reddit.com"); !strings.Contains(got, "https://old.reddit.com/r/") {
		t.Fatalf("postContent: %q", got)
	}
}

func TestCacheEvictsOldestKeys(t *testing.T) {
	c := NewCache(time.Minute, 3)
	for i := 0; i < 20; i++ {
		c.Get(fmt.Sprintf("k%d", i),
			func() []byte { return []byte("x") },
			func() []byte { return []byte("e") })
	}
	if len(c.entries) > 3 || len(c.lastSeen) > 3 || len(c.lastSuccess) > 3 {
		t.Fatalf("не ограничено: entries=%d seen=%d success=%d",
			len(c.entries), len(c.lastSeen), len(c.lastSuccess))
	}
	if c.evictions == 0 {
		t.Fatal("вытеснений не было")
	}
}

func TestCacheErrorKeysBounded(t *testing.T) {
	c := NewCache(time.Minute, 3)
	for i := 0; i < 20; i++ {
		c.Get(fmt.Sprintf("e%d", i),
			func() []byte { panic("upstream 429") },
			func() []byte { return []byte("<feed/>") })
	}
	if len(c.lastErr) > 3 || len(c.staleFlags) > 3 ||
		len(c.cooldownUntil) > 3 || len(c.lastSeen) > 3 {
		t.Fatalf("map ошибок не ограничены: err=%d stale=%d cooldown=%d seen=%d",
			len(c.lastErr), len(c.staleFlags), len(c.cooldownUntil), len(c.lastSeen))
	}
}

func TestCacheSingleflight(t *testing.T) {
	c := NewCache(time.Minute, 8)
	var mu sync.Mutex
	calls := 0
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Get("same", func() []byte {
				mu.Lock()
				calls++
				mu.Unlock()
				time.Sleep(20 * time.Millisecond)
				return []byte("x")
			}, func() []byte { return []byte("e") })
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("produce вызван %d раз, хотим 1", calls)
	}
}

func TestHistoryBounded(t *testing.T) {
	src := NewHTMLSource("x", time.Second, nil, "")
	now := float64(time.Now().Unix())
	for i := 0; i < 6000; i++ {
		src.history[fmt.Sprintf("g%d", i)] = Post{ID: fmt.Sprint(i), CreatedUTC: now - float64(i)}
	}
	src.mergeHistory(map[string]Post{}, Target{Kind: "sub", Name: "x"})
	if len(src.history) > src.maxHistory {
		t.Fatalf("история = %d, лимит %d", len(src.history), src.maxHistory)
	}
}

func TestUnwrapTextBody(t *testing.T) {
	// Воспроизводим разметку Reddit: внешний div со slot, внутри ссылка-обёртка.
	inner := `<div slot="text-body"><a slot="text-body" href="/r/x/" class="pointer-events-none">` +
		`<div class="md"><p>Hello <strong>world</strong> <a href="https://example.org">link</a></p></div>` +
		`</a></div>`
	body := slotHTML(inner, "text-body")
	if !strings.Contains(body, `slot="text-body"`) {
		t.Fatal("тест не воспроизводит баг: обёртка не найдена")
	}
	got := unwrapTextBody(body)
	if strings.Contains(got, `slot="text-body"`) {
		t.Fatalf("ссылка-обёртка осталась: %s", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "<strong>") {
		t.Fatalf("текст поста потерян: %s", got)
	}
	if !strings.Contains(got, `href="https://example.org"`) {
		t.Fatalf("вложенная ссылка потеряна: %s", got)
	}
}

func TestMediaPostHasPlainText(t *testing.T) {
	attrs := map[string]string{
		"post-type": "image", "content-href": "https://i.redd.it/x.png",
		"domain": "i.redd.it", "permalink": "/r/x/comments/1/y/",
	}
	got := contentHTML(attrs, "")
	if !strings.Contains(got, "Изображение") {
		t.Fatalf("нет обычного текста: %s", got)
	}
	if strings.Count(got, "<a ") != 1 {
		t.Fatalf("контент не должен состоять из ссылок: %s", got)
	}
}
