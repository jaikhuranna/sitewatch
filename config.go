package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Watcher struct {
	Name     string     `json:"name"`
	URL      string     `json:"url"`
	Interval int        `json:"interval,omitempty"`
	Selector string     `json:"selector,omitempty"`
	Mode     string     `json:"mode,omitempty"`
	Keywords []string   `json:"keywords,omitempty"`
	Pattern  string     `json:"pattern,omitempty"`
	Enabled  *bool      `json:"enabled,omitempty"`
	Render   bool       `json:"render,omitempty"`  // load the page in headless Chrome (runs its JavaScript)
	Items    *ItemsSpec `json:"items,omitempty"`   // extract a list of listings from the page with CSS selectors
	Include  []string   `json:"include,omitempty"` // per-watcher filter overrides (default: top-level filter)
	Exclude  []string   `json:"exclude,omitempty"`
}

// ItemsSpec picks listings out of an HTML page. Title is required. Without Item, each title's card is its
// nearest ancestor that is (or contains) the link. Detail may match several elements; their texts are
// joined with " · ". Detail and Link are looked up inside the card.
type ItemsSpec struct {
	Item   string `json:"item,omitempty"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Link   string `json:"link,omitempty"`
}

// Filter: case-insensitive RE2 patterns matched against each listing's title.
type Filter struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type Telegram struct {
	BotToken string  `json:"bot_token"`
	ChatIDs  []int64 `json:"chat_ids"` // every chat that gets alerts and may use /health
}

type Config struct {
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	AuthToken  string    `json:"auth_token"`
	NtfyTopic  string    `json:"ntfy_topic"`
	UserAgent  string    `json:"user_agent"`
	NotifyFile string    `json:"notify_file"`
	Filter     Filter    `json:"filter"`
	Telegram   *Telegram `json:"telegram"`
	Chrome     string    `json:"chrome"` // Chrome/Chromium binary for render watchers; default: search PATH
	Watchers   []Watcher `json:"watchers"`
}

// loadConfig reads config.json, then overlays the gitignored secrets.json (bot tokens etc.) if present.
func loadConfig(dir string) (Config, error) {
	cfg := Config{Host: "0.0.0.0", Port: 8478, UserAgent: "sitewatch/0.1"}
	for _, name := range []string{"config.json", "secrets.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if name == "secrets.json" && errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return cfg, err
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("%s: %w", name, err)
		}
	}
	return cfg, nil
}

// expand fills defaults, and mode/filters for item-list watchers.
func (w Watcher) expand(f Filter) (Watcher, error) {
	if w.Interval == 0 {
		w.Interval = 300
	}
	w.Interval = max(w.Interval, 10)
	if w.URL == "" {
		return w, fmt.Errorf("url is required")
	}
	if w.Items != nil {
		if w.Items.Title == "" {
			return w, fmt.Errorf("items.title selector is required")
		}
		if w.Mode == "" {
			w.Mode = "new_items"
		}
		if w.Include == nil {
			w.Include = f.Include
		}
		if w.Exclude == nil {
			w.Exclude = f.Exclude
		}
	}
	if w.Mode == "" {
		w.Mode = "changed"
	}
	return w, nil
}
