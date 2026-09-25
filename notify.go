package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

func (m *Manager) appendLine(path, s string) {
	m.fileMu.Lock()
	defer m.fileMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = f.WriteString(s)
		f.Close()
	}
	if err != nil {
		log.Printf("writing %s: %v", path, err)
	}
}

func (m *Manager) appendEvent(ev map[string]any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(ev) // appends the trailing newline
	m.appendLine(m.path("events.jsonl"), buf.String())
}

func (m *Manager) filePush(w Watcher, t *trigger) {
	m.mu.Lock()
	name := m.cfg.NotifyFile
	m.mu.Unlock()
	if name == "" {
		return
	}
	body := "  " + strings.ReplaceAll(t.excerpt, "\n", "\n  ")
	m.appendLine(m.path(name), fmt.Sprintf("[%s] %s — %s\n  %s\n%s\n\n",
		time.Now().Format("2006-01-02 15:04"), w.Name, t.detail, w.URL, body))
}

func (m *Manager) ntfyPush(name, detail string) {
	m.mu.Lock()
	topic := m.cfg.NtfyTopic
	m.mu.Unlock()
	if topic == "" {
		return
	}
	req, _ := http.NewRequest("POST", "https://ntfy.sh/"+topic, strings.NewReader(name+": "+detail))
	req.Header.Set("Title", "sitewatch")
	req.Header.Set("Priority", "high")
	req.Header.Set("Tags", "rotating_light")
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req); err == nil {
		resp.Body.Close()
	} // best effort: push must never break the check loop
}

type tgResponse struct {
	OK          bool   `json:"ok"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// telegramSend sends one message; on 429 it waits the requested time and retries once.
func (m *Manager) telegramSend(token string, chatID int64, text string) (tgResponse, error) {
	m.tgMu.Lock()
	defer m.tgMu.Unlock()
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": trunc(text, 4096),
		"disable_web_page_preview": true})
	var r tgResponse
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Post(
			"https://api.telegram.org/bot"+token+"/sendMessage", "application/json", bytes.NewReader(body))
		if ue, ok := err.(*url.Error); ok {
			return r, ue.Err // url.Error's message includes the URL, which contains the bot token
		} else if err != nil {
			return r, err
		}
		r = tgResponse{}
		json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			break
		}
		time.Sleep(time.Duration(max(r.Parameters.RetryAfter, 1)) * time.Second)
	}
	return r, nil
}

func (m *Manager) telegramPush(name string, t *trigger) {
	m.mu.Lock()
	tg := m.cfg.Telegram
	m.mu.Unlock()
	if tg == nil || tg.BotToken == "" {
		return
	}
	for _, id := range tg.ChatIDs {
		if r, err := m.telegramSend(tg.BotToken, id, name+": "+t.detail+"\n\n"+t.excerpt); err != nil || !r.OK {
			log.Printf("telegram send to %d failed: %v %s", id, err, r.Description) // best effort; notify_file keeps the record
		}
	}
}

// telegramBot long-polls the bot for commands and answers /health, only for chats in chat_ids.
// Messages from other chats are logged so a new person's chat id can be copied into chat_ids.
func (m *Manager) telegramBot() {
	client := &http.Client{Timeout: 70 * time.Second}
	offset, menuSet := 0, false
	for {
		m.mu.Lock()
		tg := m.cfg.Telegram
		m.mu.Unlock()
		if tg == nil || tg.BotToken == "" {
			time.Sleep(30 * time.Second)
			continue
		}
		api := "https://api.telegram.org/bot" + tg.BotToken + "/"
		if !menuSet { // shows /health in Telegram's command menu
			if resp, err := client.PostForm(api+"setMyCommands", url.Values{"commands": {
				`[{"command":"health","description":"Status of all watchers"}]`}}); err == nil {
				resp.Body.Close()
				menuSet = true
			}
		}
		var d struct {
			OK     bool `json:"ok"`
			Result []struct {
				UpdateID int `json:"update_id"`
				Message  *struct {
					Date int64  `json:"date"`
					Text string `json:"text"`
					Chat struct {
						ID        int64  `json:"id"`
						FirstName string `json:"first_name"`
						LastName  string `json:"last_name"`
					} `json:"chat"`
				} `json:"message"`
			} `json:"result"`
		}
		resp, err := client.Get(fmt.Sprintf("%sgetUpdates?timeout=50&offset=%d", api, offset))
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&d)
			resp.Body.Close()
		}
		if err != nil || !d.OK { // no logging: url.Error would include the bot token
			time.Sleep(15 * time.Second)
			continue
		}
		for _, u := range d.Result {
			offset = u.UpdateID + 1
			msg := u.Message
			if msg == nil {
				continue
			}
			if !slices.Contains(tg.ChatIDs, msg.Chat.ID) {
				log.Printf("telegram: unlisted chat %d (%s %s) sent %q", msg.Chat.ID, msg.Chat.FirstName, msg.Chat.LastName, trunc(msg.Text, 50))
				continue
			}
			if strings.HasPrefix(msg.Text, "/health") && time.Since(time.Unix(msg.Date, 0)) < 10*time.Minute {
				if r, err := m.telegramSend(tg.BotToken, msg.Chat.ID, m.healthReport()); err != nil || !r.OK {
					log.Printf("telegram health reply to %d failed: %v %s", msg.Chat.ID, err, r.Description)
				}
			}
		}
	}
}

// healthReport summarises every watcher: failing ones, overdue ones, and recent alerts.
func (m *Manager) healthReport() string {
	m.mu.Lock()
	total := len(m.run)
	m.mu.Unlock()
	st, _ := m.snapshot()
	now := epoch()
	ago := func(t float64) string {
		d := time.Duration(now-t) * time.Second
		if d < time.Hour {
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		}
		return fmt.Sprintf("%.1fh ago", d.Hours())
	}
	var bad, alerts []string
	healthy := 0
	names := slices.Sorted(maps.Keys(st))
	for _, name := range names {
		s := st[name]
		overdue := now-*s.LastCheck > 2.5*float64(s.Interval) // a failed check backs off to 2x the interval
		switch {
		case s.LastStatus != "ok" && s.LastStatus != "triggered":
			bad = append(bad, fmt.Sprintf("❌ %s: %s (%s)", name, s.LastStatus, ago(*s.LastCheck)))
		case overdue:
			bad = append(bad, fmt.Sprintf("⏳ %s: last checked %s", name, ago(*s.LastCheck)))
		default:
			healthy++
		}
		if s.LastTrigger != nil && now-*s.LastTrigger < 24*3600 {
			alerts = append(alerts, fmt.Sprintf("%s %s", name, ago(*s.LastTrigger)))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sitewatch: %d/%d watchers healthy", healthy, total)
	if pending := total - len(st); pending > 0 {
		fmt.Fprintf(&b, ", %d not checked yet", pending)
	}
	b.WriteString("\n")
	for _, l := range bad {
		b.WriteString("\n" + l)
	}
	if len(bad) == 0 {
		b.WriteString("\n✅ No problems")
	}
	if len(alerts) > 0 {
		b.WriteString("\n\nAlerts in the last 24h: " + strings.Join(alerts, ", "))
	}
	return b.String()
}
