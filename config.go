package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type Source struct {
	URL  string `json:"url,omitempty"`
	File string `json:"file,omitempty"`
}
type Record struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Create  bool   `json:"create_if_missing,omitempty"`
	Proxied bool   `json:"proxied,omitempty"`
	TTL     int    `json:"ttl,omitempty"`
}
type Config struct {
	ZoneID        string   `json:"zone_id"`
	Records       []Record `json:"records"`
	IPv4          Source   `json:"ipv4"`
	IPv6          Source   `json:"ipv6"`
	Interval      string   `json:"interval"`
	Timeout       string   `json:"request_timeout"`
	TokenFile     string   `json:"token_file"`
	PasswordFile  string   `json:"dashboard_password_file"`
	Username      string   `json:"dashboard_username"`
	Listen        string   `json:"listen"`
	StateDir      string   `json:"state_dir"`
	APIBase       string   `json:"-"` // injected only by tests; production always uses Cloudflare
	poll, timeout time.Duration
}

func loadConfig(path string) (Config, error) {
	c := Config{Interval: "5m", Timeout: "10s", TokenFile: "/run/secrets/cloudflare_api_token", PasswordFile: "/run/secrets/dashboard_password", Username: "admin", Listen: ":8080", StateDir: "/state", APIBase: "https://api.cloudflare.com/client/v4"}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("config must contain one JSON object")
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var err error
	c.poll, err = time.ParseDuration(c.Interval)
	if err != nil || c.poll < time.Second {
		return fmt.Errorf("interval must be at least 1s")
	}
	c.timeout, err = time.ParseDuration(c.Timeout)
	if err != nil || c.timeout < time.Second || c.timeout > time.Minute {
		return fmt.Errorf("request_timeout must be between 1s and 1m")
	}
	if !regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(c.ZoneID) {
		return fmt.Errorf("zone_id must be a 32-character hex ID")
	}
	if len(c.Records) == 0 {
		return fmt.Errorf("at least one record is required")
	}
	seen := map[string]bool{}
	need4, need6 := false, false
	for i := range c.Records {
		r := &c.Records[i]
		r.Name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.Name), "."))
		r.Type = strings.ToUpper(r.Type)
		if len(r.Name) > 253 || !regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(r.Name) {
			return fmt.Errorf("invalid record hostname %q", r.Name)
		}
		if r.Type != "A" && r.Type != "AAAA" {
			return fmt.Errorf("record type must be A or AAAA")
		}
		k := r.Name + "/" + r.Type
		if seen[k] {
			return fmt.Errorf("duplicate configured record %s", k)
		}
		seen[k] = true
		need4 = need4 || r.Type == "A"
		need6 = need6 || r.Type == "AAAA"
		if r.TTL == 0 {
			r.TTL = 1
		}
		if r.TTL != 1 && (r.TTL < 60 || r.TTL > 86400) {
			return fmt.Errorf("creation TTL must be 1 (automatic) or 60..86400")
		}
	}
	if need4 && c.IPv4 == (Source{}) {
		c.IPv4.URL = "https://api.ipify.org"
	}
	for _, v := range []struct {
		s      Source
		needed bool
		label  string
	}{{c.IPv4, need4, "ipv4"}, {c.IPv6, need6, "ipv6"}} {
		if v.s == (Source{}) && !v.needed {
			continue
		}
		if (v.s.URL == "") == (v.s.File == "") {
			return fmt.Errorf("%s requires exactly one URL or file source", v.label)
		}
		if v.s.URL != "" {
			u, e := url.Parse(v.s.URL)
			if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
				return fmt.Errorf("invalid %s source URL", v.label)
			}
		}
	}
	if c.Username == "" || strings.Contains(c.Username, ":") || c.Listen == "" || c.StateDir == "" || c.TokenFile == "" || c.PasswordFile == "" {
		return fmt.Errorf("listen, state_dir, secret files and a valid dashboard_username are required")
	}
	return nil
}

func readSecret(path string) (string, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return "", fmt.Errorf("cannot read secret file %s", path)
	}
	s := strings.TrimSpace(string(b))
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("secret file must contain one nonempty line")
	}
	return s, nil
}
