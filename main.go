// sitewatch watches web pages and listings per config.json and alerts via Telegram, ntfy, and a text
// file. The same process serves a phone PWA and JSON API, meant to be reached over Tailscale.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

//go:embed web
var webFiles embed.FS

func main() {
	dir := flag.String("dir", ".", "directory with config.json; runtime files are written here too")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sitewatch [-dir DIR]                         run the watcher + web app")
		fmt.Fprintln(os.Stderr, "       sitewatch [-dir DIR] telegram-setup <token>  link a Telegram bot")
		fmt.Fprintln(os.Stderr, "       sitewatch [-dir DIR] preview <watcher>       show what a watcher extracts (no alerts, no state)")
		fmt.Fprintln(os.Stderr, "       sitewatch [-dir DIR] rebaseline              mark everything listed now as seen (service stopped)")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.Arg(0) == "rebaseline" {
		if err := rebaseline(*dir); err != nil {
			log.Fatal(err)
		}
		return
	}
	if flag.Arg(0) == "preview" {
		if err := preview(*dir, flag.Arg(1)); err != nil {
			log.Fatal(err)
		}
		return
	}
	if flag.Arg(0) == "telegram-setup" {
		if err := telegramSetup(*dir, flag.Arg(1)); err != nil {
			log.Fatal(err)
		}
		return
	}

	cfg, err := loadConfig(*dir)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if len(cfg.AuthToken) < 16 || strings.HasPrefix(cfg.AuthToken, "change-me") {
		log.Fatal(`set a long random "auth_token" in secrets.json (the web app is reachable on the network)`)
	}
	m := newManager(*dir)
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))) // bind first so watchers never race the listener
	if err != nil {
		log.Fatal(err)
	}
	m.start(cfg)
	go m.telegramBot()
	web, _ := fs.Sub(webFiles, "web")
	fmt.Printf("Serving on http://%s:%d — use http://<your-tailscale-ip>:%d on your phone\n", cfg.Host, cfg.Port, cfg.Port)
	log.Fatal(http.Serve(ln, (&server{m: m, token: cfg.AuthToken, web: web}).routes()))
}

// preview runs one extraction for a watcher and prints the result, for tuning selectors.
func preview(dir, name string) error {
	cfg, err := loadConfig(dir)
	if err != nil {
		return err
	}
	for _, w := range cfg.Watchers {
		if w.Name != name {
			continue
		}
		if w, err = w.expand(cfg.Filter); err != nil {
			return err
		}
		m := newManager(dir)
		m.cfg = cfg
		text, items, _, status, err := m.extract(w)
		fmt.Printf("status: %s  error: %v  url: %s\n", status, err, w.URL)
		if w.Items != nil {
			fmt.Printf("%d items after filter:\n%s\n", len(items), text)
		} else {
			fmt.Println(trunc(text, 2000))
		}
		return nil
	}
	return fmt.Errorf("no watcher named %q in config.json", name)
}

// rebaseline marks every item currently listed as seen, without alerting. Use it after code changes
// that alter line formats; config edits are handled live by Manager.absorb.
func rebaseline(dir string) error {
	cfg, err := loadConfig(dir)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return fmt.Errorf("port %d is busy; stop the service first: systemctl --user stop sitewatch", cfg.Port)
	}
	ln.Close()
	m := newManager(dir)
	m.cfg = cfg
	m.loadState()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, w := range cfg.Watchers {
		if w.Enabled != nil && !*w.Enabled {
			continue
		}
		if w, err = w.expand(cfg.Filter); err != nil || w.Mode != "new_items" {
			continue
		}
		wg.Add(1)
		go func(w Watcher) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, items, _, status, err := m.extract(w)
			if status != "ok" {
				fmt.Printf("%-14s skipped: %s %v\n", w.Name, status, err)
				return
			}
			m.mu.Lock()
			set := m.seen[w.Name]
			if set == nil {
				set = map[string]bool{}
				m.seen[w.Name] = set
			}
			for _, it := range items {
				set[it] = true
			}
			m.mu.Unlock()
			fmt.Printf("%-14s %d matching items marked seen\n", w.Name, len(items))
		}(w)
	}
	wg.Wait()
	m.mu.Lock()
	m.saveState()
	m.mu.Unlock()
	return nil
}

// telegramSetup finds the chat of whoever last messaged the bot, adds it to chat_ids in secrets.json
// (keeping existing chats when the token is unchanged), and sends a test message. Message the bot first
// (press Start / send "hi"), and stop the service: while it runs, its /health loop consumes the messages.
func telegramSetup(dir, token string) error {
	if token == "" {
		return fmt.Errorf("usage: sitewatch telegram-setup <bot_token>")
	}
	resp, err := http.Get("https://api.telegram.org/bot" + token + "/getUpdates")
	if err != nil {
		return fmt.Errorf("reaching Telegram failed")
	}
	defer resp.Body.Close()
	var d struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      []struct {
			Message *struct {
				Chat struct {
					ID        int64  `json:"id"`
					FirstName string `json:"first_name"`
					Title     string `json:"title"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil || !d.OK {
		return fmt.Errorf("Telegram rejected the token: %s", d.Description)
	}
	var chatID int64
	var who string
	for _, u := range d.Result {
		if u.Message != nil {
			chatID, who = u.Message.Chat.ID, u.Message.Chat.FirstName+u.Message.Chat.Title
		}
	}
	if chatID == 0 {
		return fmt.Errorf("no messages found: open your bot in Telegram, press Start / send \"hi\", then rerun")
	}

	path := filepath.Join(dir, "secrets.json")
	secrets := map[string]any{}
	var old struct{ Telegram Telegram }
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &secrets); err != nil {
			return fmt.Errorf("secrets.json: %w", err)
		}
		json.Unmarshal(b, &old)
	}
	var ids []int64
	if old.Telegram.BotToken == token {
		ids = old.Telegram.ChatIDs
	}
	if !slices.Contains(ids, chatID) {
		ids = append(ids, chatID)
	}
	secrets["telegram"] = Telegram{BotToken: token, ChatIDs: ids}
	b, _ := json.MarshalIndent(secrets, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	os.Chmod(path, 0o600) // WriteFile keeps an existing file's mode
	r, err := newManager(dir).telegramSend(token, chatID, "sitewatch is connected. Alerts will arrive here.")
	if err != nil || !r.OK {
		return fmt.Errorf("saved chat %s (%d), but the test message failed: %v %s", who, chatID, err, r.Description)
	}
	fmt.Printf("Saved chat %s (%d) to secrets.json; test message sent\n", who, chatID)
	return nil
}
