package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestDashboardDirectAccessCSRFProgressAndHealth(t *testing.T) {
	f, u := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := dashboard(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	request := func(method, path, csrf, origin string) (int, string) {
		t.Helper()
		r, _ := http.NewRequest(method, server.URL+path, nil)
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
		if resp.Header.Get("WWW-Authenticate") != "" {
			t.Fatal("dashboard must not request sign-in")
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	if code, _ := request("GET", "/", "", ""); code != 200 {
		t.Fatal(code)
	}
	if code, _ := request("GET", "/api/status", "", ""); code != 200 {
		t.Fatal(code)
	}
	if code, _ := request("GET", "/healthz", "", ""); code != 503 {
		t.Fatal("health must start unhealthy")
	}
	code, html := request("GET", "/", "", "")
	if code != 200 || !strings.Contains(html, "Force Update") || strings.Contains(html, "test-secret") || strings.Contains(html, `id="ipv6"`) || strings.Contains(html, "el('ipv6')") {
		t.Fatal("dashboard page invalid")
	}
	token := regexp.MustCompile(`const csrf='([a-f0-9]+)'`).FindStringSubmatch(html)
	if len(token) != 2 {
		t.Fatal("missing CSRF token")
	}
	if code, _ := request("POST", "/api/force", "", ""); code != 403 {
		t.Fatal(code)
	}
	if code, _ := request("POST", "/api/force", token[1], "https://evil.example"); code != 403 {
		t.Fatal(code)
	}
	if code, _ := request("GET", "/api/force", token[1], ""); code != 405 {
		t.Fatal(code)
	}
	if !u.begin(false) {
		t.Fatal("begin failed")
	}
	if code, _ := request("POST", "/api/force", token[1], server.URL); code != 409 {
		t.Fatal(code)
	}
	_, body := request("GET", "/api/status", "", "")
	var status Status
	if json.Unmarshal([]byte(body), &status) != nil || !status.Running {
		t.Fatal("progress not visible")
	}
	if err := u.finish(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if code, _ := request("GET", "/healthz", "", ""); code != 200 {
		t.Fatal(code)
	}
	if code, _ := request("POST", "/api/force", token[1], server.URL); code != 202 {
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
	if code, _ := request("GET", "/healthz", "", ""); code != 503 {
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

func TestConfigWithoutDashboardCredentials(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "no credentials", true: "ignored legacy credentials"}[legacy], func(t *testing.T) {
			config := map[string]any{"zone_id": strings.Repeat("a", 32), "records": []Record{{Name: "home.example.com", Type: "A"}}}
			if legacy {
				config["dashboard_username"] = "old:username"
				config["dashboard_password_file"] = filepath.Join(t.TempDir(), "missing-password")
			}
			data, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(p, data, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := loadConfig(p)
			if err != nil {
				t.Fatal(err)
			}
			if c.Listen != "127.0.0.1:8080" {
				t.Fatalf("unexpected default listen: %s", c.Listen)
			}
		})
	}
}
