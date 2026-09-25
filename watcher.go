package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

type Status struct {
	Mode        string   `json:"mode"`
	Interval    int      `json:"interval"`
	URL         string   `json:"url"`
	LastCheck   *float64 `json:"last_check"`
	LastStatus  string   `json:"last_status"`
	LastTrigger *float64 `json:"last_trigger"`
	Checks      int      `json:"checks"`
}

type running struct {
	w    Watcher
	stop chan struct{}
}

type trigger struct{ detail, excerpt string }

// Manager owns all watcher goroutines and shared state. mu guards every field below it.
type Manager struct {
	dir    string
	mu     sync.Mutex
	cfg    Config                     // runtime settings: user agent, sinks, filter
	run    map[string]*running        // active watchers
	status map[string]*Status         // per-watcher status for the API
	hashes map[string]string          // last-triggered (or baseline) sha256 of extracted text
	seen   map[string]map[string]bool // new_items: lines already seen, persisted to state.json
	reload float64

	fileMu sync.Mutex // serializes appends to events.jsonl and notify_file
	tgMu   sync.Mutex // serializes Telegram sends so bursts don't hit its per-chat rate limit
}

func newManager(dir string) *Manager {
	return &Manager{dir: dir, run: map[string]*running{}, status: map[string]*Status{},
		hashes: map[string]string{}, seen: map[string]map[string]bool{}}
}

// fetchClient: 20s to start responding, 90s total for large pages.
var fetchClient = &http.Client{Timeout: 90 * time.Second,
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 20 * time.Second,
		TLSHandshakeTimeout: 20 * time.Second, MaxIdleConnsPerHost: 4}}

func epoch() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// trunc cuts s to at most n runes (never splits a UTF-8 character).
func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (m *Manager) path(name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(m.dir, name)
}

func (m *Manager) fetchPage(url string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	req.Header.Set("User-Agent", m.cfg.UserAgent)
	m.mu.Unlock()
	resp, err := fetchClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

// anyOf compiles patterns into one case-insensitive regexp; nil when there are none.
func anyOf(patterns []string) (*regexp.Regexp, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	return regexp.Compile("(?i)(?:" + strings.Join(patterns, ")|(?:") + ")")
}

// filterItems applies the watcher's include/exclude title patterns -> sorted, deduped
// "title | detail | link" lines.
func filterItems(list []item, w Watcher) ([]string, error) {
	inc, err := anyOf(w.Include)
	if err != nil {
		return nil, fmt.Errorf("include: %w", err)
	}
	exc, err := anyOf(w.Exclude)
	if err != nil {
		return nil, fmt.Errorf("exclude: %w", err)
	}
	set := map[string]bool{}
	for _, it := range list {
		title := clean(it.Title)
		if title == "" || inc != nil && !inc.MatchString(title) || exc != nil && exc.MatchString(title) {
			continue
		}
		set[title+" | "+clean(it.Detail)+" | "+strings.TrimSpace(it.Link)] = true
	}
	return slices.Sorted(maps.Keys(set)), nil
}

// nodeText joins a node's non-empty text fragments with spaces (like BeautifulSoup get_text(" ", strip=True)).
func nodeText(n *html.Node) string {
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				parts = append(parts, t)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(parts, " ")
}

// extract returns the watcher's text, or a non-ok status. For item lists it also returns the lines and
// the unfiltered items.
func (m *Manager) extract(w Watcher) (text string, lines []string, list []item, status string, err error) {
	var page string
	if w.Render {
		page, err = m.render(w.URL)
	} else {
		page, err = m.fetchPage(w.URL)
	}
	if err != nil {
		return "", nil, nil, "fetch_error", err
	}
	if w.Items != nil {
		if list, err = pageItems(page, w.URL, w.Items); err != nil {
			return "", nil, nil, "fetch_error", err
		}
		if len(list) == 0 { // layout changed, or the page didn't finish rendering / was blocked
			return "", nil, nil, "selector_missing", nil
		}
		if lines, err = filterItems(list, w); err != nil {
			return "", nil, nil, "config_error", err
		}
		return strings.Join(lines, "\n"), lines, list, "ok", nil
	}
	if w.Selector == "" {
		return page, nil, nil, "ok", nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		return "", nil, nil, "fetch_error", err
	}
	sel := doc.Find(w.Selector).First()
	if sel.Length() == 0 {
		return "", nil, nil, "selector_missing", nil
	}
	return nodeText(sel.Nodes[0]), nil, nil, "ok", nil
}

func evaluate(w Watcher, text string) (*trigger, error) {
	switch w.Mode {
	case "contains":
		var hits []string
		for _, k := range w.Keywords {
			if strings.Contains(text, k) {
				hits = append(hits, k)
			}
		}
		if len(hits) > 0 {
			return &trigger{"keywords matched: " + strings.Join(hits, ", "), trunc(text, 300)}, nil
		}
	case "not_contains":
		if !slices.ContainsFunc(w.Keywords, func(k string) bool { return strings.Contains(text, k) }) {
			return &trigger{"none of the keywords present", trunc(text, 300)}, nil
		}
	case "regex":
		re, err := regexp.Compile(w.Pattern)
		if err != nil {
			return nil, err
		}
		if loc := re.FindStringIndex(text); loc != nil {
			return &trigger{fmt.Sprintf("pattern %q matched", w.Pattern), trunc(text[loc[0]:loc[1]], 200)}, nil
		}
	default:
		return nil, fmt.Errorf("unknown mode %q", w.Mode)
	}
	return nil, nil
}

// saveState persists seen lines atomically. Call with mu held.
func (m *Manager) saveState() {
	out := map[string][]string{}
	for name, set := range m.seen {
		out[name] = append([]string{}, slices.Sorted(maps.Keys(set))...) // [] not null for empty lists
	}
	b, _ := json.Marshal(out)
	tmp := m.path("state.tmp")
	err := os.WriteFile(tmp, b, 0o644)
	if err == nil {
		err = os.Rename(tmp, m.path("state.json"))
	}
	if err != nil {
		log.Printf("saving state: %v", err)
	}
}

// newItems triggers on lines not seen before. The first run baselines silently. With keep, lines are
// never forgotten: pages usually show only the first page of results, so a listing can drop off and come back.
func (m *Manager) newItems(name string, items []string, keep bool) *trigger {
	m.mu.Lock()
	prev, had := m.seen[name]
	if len(items) == 0 && len(prev) > 0 {
		m.mu.Unlock()
		return nil // an empty response is almost always a glitch; don't wipe the baseline
	}
	cur := make(map[string]bool, len(items))
	for _, it := range items {
		cur[it] = true
	}
	if keep {
		maps.Copy(cur, prev)
	}
	if !had || !maps.Equal(prev, cur) {
		m.seen[name] = cur
		m.saveState()
	}
	m.mu.Unlock()
	if !had {
		return nil
	}
	var fresh []string
	for _, it := range items {
		if !prev[it] {
			fresh = append(fresh, it)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	first, _, _ := strings.Cut(fresh[0], " | ")
	more := ""
	if len(fresh) > 1 {
		more = fmt.Sprintf(" (+%d more)", len(fresh)-1)
	}
	return &trigger{fmt.Sprintf("%d new: %s%s", len(fresh), first, more), trunc(strings.Join(fresh, "\n"), 3000)}
}

func sameSource(a, b Watcher) bool {
	return a.URL == b.URL && a.Render == b.Render && reflect.DeepEqual(a.Items, b.Items)
}

// absorb quietly marks listings as seen when a config edit (e.g. a looser filter) made existing
// listings match. Listings the old config would also have matched still alert as usual. If the source
// itself changed, lines aren't comparable, so everything current is absorbed.
func (m *Manager) absorb(w, old Watcher, list []item, items []string) {
	quiet := items
	if sameSource(w, old) && old.Mode == "new_items" {
		if was, err := filterItems(list, old); err == nil {
			quiet = slices.DeleteFunc(slices.Clone(items), func(it string) bool { return slices.Contains(was, it) })
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, had := m.seen[w.Name]; had && len(quiet) > 0 {
		for _, it := range quiet {
			prev[it] = true
		}
		m.saveState()
	}
}

// check does one fetch/extract/evaluate pass and returns the resulting status. old is the watcher's
// previous config if it was just edited (see absorb), else nil.
func (m *Manager) check(w Watcher, old *Watcher) string {
	text, items, list, status, err := m.extract(w)
	var trig *trigger
	if status == "ok" {
		sum := sha256.Sum256([]byte(text))
		h := hex.EncodeToString(sum[:])
		m.mu.Lock()
		last, had := m.hashes[w.Name]
		m.mu.Unlock()
		switch {
		case w.Mode == "new_items":
			if old != nil {
				m.absorb(w, *old, list, items)
			}
			trig = m.newItems(w.Name, items, w.Items != nil)
		case w.Mode == "changed" && !had:
			m.mu.Lock()
			m.hashes[w.Name] = h // first run: silent baseline
			m.mu.Unlock()
		case w.Mode == "changed" && h != last:
			trig = &trigger{"content changed", trunc(text, 300)}
		case w.Mode != "changed" && h != last: // dedup: never re-trigger on the last-triggered text
			if trig, err = evaluate(w, text); err != nil {
				status = "config_error"
			}
		}
		if trig != nil && w.Mode != "new_items" {
			m.mu.Lock()
			m.hashes[w.Name] = h
			m.mu.Unlock()
		}
	}
	if err != nil {
		log.Printf("%s: %s: %v", w.Name, status, err)
	}

	now := epoch()
	if trig != nil {
		m.appendEvent(map[string]any{"ts": now, "watcher": w.Name, "mode": w.Mode, "url": w.URL,
			"detail": trig.detail, "excerpt": trig.excerpt})
		m.filePush(w, trig)
		m.ntfyPush(w.Name, trig.detail)
		m.telegramPush(w.Name, trig)
		status = "triggered"
	}
	m.mu.Lock()
	if s := m.status[w.Name]; s != nil {
		s.LastCheck, s.LastStatus, s.Checks = &now, status, s.Checks+1
		if trig != nil {
			s.LastTrigger = &now
		}
	}
	m.mu.Unlock()
	return status
}

func (m *Manager) loop(w Watcher, stop chan struct{}, old *Watcher) {
	for {
		wait := time.Duration(w.Interval) * time.Second
		switch m.check(w, old) {
		case "fetch_error":
			wait *= 2 // back off once after an error, then go back to the normal interval
		case "ok", "triggered":
			old = nil // the edit is handled once a check succeeds
		}
		select {
		case <-stop:
			return
		case <-time.After(wait):
		}
	}
}

// apply loads runtime settings and starts/stops/restarts watchers to match cfg.
func (m *Manager) apply(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	m.reload = epoch()
	want := map[string]Watcher{}
	for _, w := range cfg.Watchers {
		if w.Enabled != nil && !*w.Enabled {
			continue
		}
		ew, err := w.expand(cfg.Filter)
		if err != nil {
			log.Printf("%s: skipped: %v", w.Name, err)
			continue
		}
		want[w.Name] = ew
	}
	edited := map[string]*Watcher{}
	for name, r := range m.run {
		if w, ok := want[name]; !ok || !reflect.DeepEqual(w, r.w) {
			close(r.stop) // removed or changed: stop it; changed ones restart below
			delete(m.run, name)
			if !ok {
				delete(m.status, name)
			} else {
				edited[name] = &r.w
			}
		}
	}
	for name, w := range want {
		if m.run[name] != nil {
			continue
		}
		r := &running{w, make(chan struct{})}
		m.run[name] = r
		s := m.status[name]
		if s == nil {
			s = &Status{LastStatus: "ok"}
			m.status[name] = s
		}
		s.Mode, s.Interval, s.URL = w.Mode, w.Interval, w.URL
		go m.loop(w, r.stop, edited[name])
	}
}

// start loads saved new_items baselines, starts watchers, and hot-reloads config.json every 30s.
// host/port/auth_token changes need a restart.
func (m *Manager) start(cfg Config) {
	m.loadState()
	m.apply(cfg)
	go func() {
		for range time.Tick(30 * time.Second) {
			cfg, err := loadConfig(m.dir)
			if err != nil {
				log.Printf("config reload skipped: %v", err) // mid-edit or invalid: keep the old one
				continue
			}
			m.apply(cfg)
		}
	}()
}

func (m *Manager) loadState() {
	if b, err := os.ReadFile(m.path("state.json")); err == nil {
		var saved map[string][]string
		if err := json.Unmarshal(b, &saved); err != nil {
			log.Printf("ignoring unreadable state.json: %v", err)
		}
		for name, lines := range saved {
			set := map[string]bool{}
			for _, l := range lines {
				set[l] = true
			}
			m.seen[name] = set
		}
	}
}

// force runs one immediate check of a watcher. False if it isn't running.
func (m *Manager) force(name string) bool {
	m.mu.Lock()
	r := m.run[name]
	m.mu.Unlock()
	if r == nil {
		return false
	}
	go m.check(r.w, nil)
	return true
}

func (m *Manager) snapshot() (map[string]Status, float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Status, len(m.status))
	for name, s := range m.status {
		if s.Checks > 0 { // not yet checked: nothing useful to show
			out[name] = *s
		}
	}
	return out, m.reload
}
