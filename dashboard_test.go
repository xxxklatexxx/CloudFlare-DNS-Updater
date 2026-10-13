package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDashboardAuthenticationCSRFProgressAndHealth(t *testing.T) {
	f, u := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := dashboard(ctx, u, "correct-long-password")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	request := func(method, path, csrf, origin string, auth bool) (int, string) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+path, nil)
		if auth {
			r.SetBasicAuth("admin", "correct-long-password")
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		resp, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if code, _ := request("GET", "/", "", "", false); code != 401 {
		t.Fatal(code)
	}
	if code, _ := request("GET", "/api/status", "", "", false); code != 401 {
		t.Fatal(code)
	}
	if code, _ := request("GET", "/healthz", "", "", false); code != 503 {
		t.Fatal("health must start unhealthy")
	}
	code, html := request("GET", "/", "", "", true)
	if code != 200 || !strings.Contains(html, "Force Update") || strings.Contains(html, "test-secret") {
		t.Fatal("dashboard page invalid")
	}
	token := regexp.MustCompile(`const csrf='([a-f0-9]+)'`).FindStringSubmatch(html)
	if len(token) != 2 {
		t.Fatal("missing CSRF token")
	}
	if code, _ := request("POST", "/api/force", "", "", true); code != 403 {
		t.Fatal(code)
	}
	if code, _ := request("POST", "/api/force", token[1], "https://evil.example", true); code != 403 {
		t.Fatal(code)
	}
	if code, _ := request("GET", "/api/force", token[1], "", true); code != 405 {
		t.Fatal(code)
	}
	if !u.begin(false) {
		t.Fatal("begin failed")
	}
	if code, _ := request("POST", "/api/force", token[1], server.URL, true); code != 409 {
		t.Fatal(code)
	}
	_, body := request("GET", "/api/status", "", "", true)
	var status Status
	if json.Unmarshal([]byte(body), &status) != nil || !status.Running {
		t.Fatal("progress not visible")
	}
	if err := u.finish(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if code, _ := request("GET", "/healthz", "", "", false); code != 200 {
		t.Fatal(code)
	}
	if code, _ := request("POST", "/api/force", token[1], server.URL, true); code != 202 {
		t.Fatal(code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for u.snapshot().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if u.snapshot().Running {
		t.Fatal("force did not finish")
	}
	f.mu.Lock()
	patches := f.patches
	f.mu.Unlock()
	if patches != 2 {
		t.Fatal("dashboard force must patch unchanged IP")
	}
	u.mu.Lock()
	u.status.LastSuccess = time.Now().Add(-20 * time.Minute)
	u.mu.Unlock()
	if code, _ := request("GET", "/healthz", "", "", false); code != 503 {
		t.Fatal("stale check must be unhealthy")
	}
}

func TestConfigValidation(t *testing.T) {
	_, u := setup(t)
	for _, mutate := range []func(*Config){func(c *Config) { c.ZoneID = "bad" }, func(c *Config) { c.Records = append(c.Records, c.Records[0]) }, func(c *Config) { c.Records = []Record{{Name: "home.example.com", Type: "AAAA"}}; c.IPv6 = Source{} }, func(c *Config) { c.Records = []Record{{Name: "home.example.com", Type: "TXT"}} }, func(c *Config) { c.IPv4 = Source{URL: "http://x", File: "x"} }, func(c *Config) { c.Timeout = "0s" }} {
		c := u.cfg
		c.Records = append([]Record{}, u.cfg.Records...)
		mutate(&c)
		if c.validate() == nil {
			t.Fatal("invalid config accepted")
		}
	}
}
