package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// One Chrome at a time: each render is a short-lived process (~300-500 MB while loading, 0 after),
// so render watchers queue up instead of stacking browsers in RAM.
var renderSem = make(chan struct{}, 1)

func findChrome() string {
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// render loads pageURL in headless Chrome, lets its JavaScript run, and returns the resulting DOM.
func (m *Manager) render(pageURL string) (string, error) {
	renderSem <- struct{}{}
	defer func() { <-renderSem }()
	m.mu.Lock()
	bin := m.cfg.Chrome
	m.mu.Unlock()
	if bin == "" {
		if bin = findChrome(); bin == "" {
			return "", errors.New(`no Chrome/Chromium found; set "chrome" in config.json`)
		}
	}
	profile, err := os.MkdirTemp("", "sitewatch-chrome-") // throwaway profile: never touches the user's Chrome
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(profile)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--headless=new", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check", "--disable-extensions", "--mute-audio",
		"--disable-crash-reporter", "--disable-breakpad", // don't touch ~/.config/google-chrome/Crash Reports
		"--blink-settings=imagesEnabled=false", "--user-data-dir="+profile, "--window-size=1400,4000",
		"--virtual-time-budget=20000", // let scripts/XHRs settle; virtual time runs faster when idle
		"--dump-dom", pageURL)
	// Own process group, so a timeout kills Chrome's renderer/GPU children too.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // reap any stragglers; ESRCH when all exited
	if err != nil {
		return "", fmt.Errorf("chrome: %w", err)
	}
	return out.String(), nil
}

func clean(s string) string { return strings.Join(strings.Fields(s), " ") }

type item struct{ Title, Detail, Link string }

// pageItems extracts listings from an HTML page per spec, resolving links against the page URL.
func pageItems(page, pageURL string, spec *ItemsSpec) ([]item, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	if b, ok := doc.Find("base[href]").Attr("href"); ok {
		if u, err := base.Parse(b); err == nil {
			base = u
		}
	}
	linkSel := spec.Link
	if linkSel == "" {
		linkSel = "a[href]"
	}
	var out []item
	add := func(card, title *goquery.Selection) {
		t := clean(title.Text())
		if t == "" {
			return
		}
		var details []string
		if spec.Detail != "" {
			card.Find(spec.Detail).Each(func(_ int, d *goquery.Selection) {
				if t := clean(d.Text()); t != "" {
					details = append(details, t)
				}
			})
		}
		a := card.Find(linkSel).First()
		if card.Is(linkSel) {
			a = card
		} else if spec.Link == "" && title.Closest("a[href]").Length() > 0 {
			a = title.Closest("a[href]")
		}
		href, _ := a.Attr("href")
		if u, err := base.Parse(href); err == nil && href != "" {
			u.RawQuery, u.Fragment = "", "" // search/tracking params would make the same listing look new
			href = u.String()
		}
		out = append(out, item{t, strings.Join(details, " · "), href})
	}
	if spec.Item != "" {
		doc.Find(spec.Item).Each(func(_ int, card *goquery.Selection) {
			add(card, card.Find(spec.Title).First())
		})
		return out, nil
	}
	doc.Find(spec.Title).Each(func(_ int, title *goquery.Selection) {
		card := title
		for p := title; p.Length() > 0; p = p.Parent() { // nearest ancestor that is/contains the link
			if p.Is(linkSel) || p.Find(linkSel).Length() > 0 {
				card = p
				break
			}
		}
		add(card, title)
	})
	return out, nil
}
