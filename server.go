package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
)

type server struct {
	m     *Manager
	token string // boot-time only; config reload ignores it
	web   fs.FS
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.file("index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /manifest.json", s.file("manifest.json", "application/manifest+json"))
	mux.HandleFunc("GET /sw.js", s.file("sw.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /demo-page", demoPage)
	mux.HandleFunc("GET /api/status", s.auth(s.apiStatus))
	mux.HandleFunc("GET /api/events", s.auth(s.apiEvents))
	mux.HandleFunc("POST /api/check/{name}", s.auth(s.apiCheck))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *server) file(name, ctype string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(s.web, name)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Write(b)
	}
}

// demoPage contains TRIGGER-WORD only with ?on=1, so the demo-local watcher works out of the box.
func demoPage(w http.ResponseWriter, r *http.Request) {
	word := "nothing to see"
	if r.URL.Query().Get("on") == "1" {
		word = "TRIGGER-WORD"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte("<!doctype html><html><body><p>" + word + "</p></body></html>"))
}

// auth accepts "Authorization: Bearer <token>" or ?token=, compared in constant time.
func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			given = r.URL.Query().Get("token")
		}
		if s.token == "" || subtle.ConstantTimeCompare([]byte(given), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *server) apiStatus(w http.ResponseWriter, r *http.Request) {
	statuses, reloaded := s.m.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"now": epoch(), "watchers": statuses, "config_reload": reloaded})
}

func (s *server) apiEvents(w http.ResponseWriter, r *http.Request) {
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit < 1 {
		limit = 50
	}
	events, err := readEvents(s.m.path("events.jsonl"), min(limit, 200))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *server) apiCheck(w http.ResponseWriter, r *http.Request) {
	if s.m.force(r.PathValue("name")) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown watcher"})
}

// readEvents returns up to limit events, newest first, looking at most at the last 1000 lines.
func readEvents(path string, limit int) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	lines = lines[max(0, len(lines)-1000):]
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		if json.Valid(lines[i]) { // skip a torn line from a concurrent append
			out = append(out, json.RawMessage(lines[i]))
		}
	}
	return out, nil
}
