package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"time"
)

//go:embed dashboard.html
var assets embed.FS

func dashboard(parent context.Context, u *Updater) (http.Handler, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	csrf := hex.EncodeToString(b)
	tpl, err := template.ParseFS(assets, "dashboard.html")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		s := u.snapshot()
		if s.LastSuccess.IsZero() || time.Since(s.LastSuccess) > 2*u.cfg.poll+time.Minute || s.Error != "" {
			http.Error(w, "unhealthy", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(u.snapshot())
	})
	mux.HandleFunc("/api/force", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrf)) != 1 {
			http.Error(w, "invalid CSRF token", 403)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			o, e := url.Parse(origin)
			if e != nil || o.Host != r.Host || (o.Scheme != "http" && o.Scheme != "https") {
				http.Error(w, "invalid origin", 403)
				return
			}
		}
		if !u.begin(true) {
			http.Error(w, "a check is already running", 409)
			return
		}
		// Bound the full manual action, independent of the browser connection.
		go func() {
			ctx, cancel := context.WithTimeout(parent, cycleBudget(u.cfg))
			defer cancel()
			_ = u.finish(ctx, true)
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		w.Write([]byte(`{"accepted":true}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = tpl.Execute(w, map[string]string{"CSRF": csrf})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+csrf+"'; style-src 'nonce-"+csrf+"'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		mux.ServeHTTP(w, r)
	}), nil
}
func cycleBudget(c Config) time.Duration {
	return time.Duration(len(c.Records)*2+2) * (3*c.timeout + 60*time.Second)
}
