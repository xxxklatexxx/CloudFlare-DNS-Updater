package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type DNSRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
}
type API struct {
	base, token, zone string
	client            *http.Client
	wait              func(context.Context, time.Duration) error
	notBefore         time.Time // accessed only by serialized reconciliation
}

func newClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func waitContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Retries only transient failures, with at most three attempts and a 30s wait cap.
// Response bodies and request URLs are never included in errors (may contain secrets).
func (a *API) request(ctx context.Context, method, path string, body any, out any) error {
	if time.Now().Before(a.notBefore) {
		return fmt.Errorf("Cloudflare rate-limit cooldown active")
	}
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, a.base+path, bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("invalid API request")
		}
		req.Header.Set("Authorization", "Bearer "+a.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := a.client.Do(req)
		retry := err != nil
		delay := time.Second * time.Duration(1<<attempt)
		if err == nil {
			b, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
			resp.Body.Close()
			retry = resp.StatusCode == 429 || resp.StatusCode >= 500
			if retry {
				if n, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && n >= 0 {
					delay = time.Duration(n) * time.Second
				} else if t, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
					delay = time.Until(t)
				}
				if resp.StatusCode == 429 {
					if delay < 0 {
						delay = 0
					}
					a.notBefore = time.Now().Add(delay)
					// Long server cooldowns defer work without retrying early.
					if delay > 30*time.Second {
						return fmt.Errorf("Cloudflare rate limited; deferred until cooldown expires")
					}
				}
			}
			if !retry {
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					return fmt.Errorf("Cloudflare HTTP %d", resp.StatusCode)
				}
				if readErr != nil || len(b) > 1<<20 {
					return fmt.Errorf("invalid Cloudflare response")
				}
				var env struct {
					Success bool            `json:"success"`
					Result  json.RawMessage `json:"result"`
				}
				if json.Unmarshal(b, &env) != nil || !env.Success {
					return fmt.Errorf("Cloudflare rejected request")
				}
				if out != nil && json.Unmarshal(env.Result, out) != nil {
					return fmt.Errorf("invalid Cloudflare result")
				}
				return nil
			}
			err = fmt.Errorf("Cloudflare HTTP %d", resp.StatusCode)
		} else {
			err = fmt.Errorf("Cloudflare connection failed")
		}
		// Creation is not idempotent: a timeout/5xx may follow a committed POST.
		// Only a rejected (429) creation request can be safely retried.
		if method == "POST" && (resp == nil || resp.StatusCode != 429) {
			return err
		}
		if attempt == 2 {
			return err
		}
		if delay < 0 {
			delay = 0
		}
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		if e := a.wait(ctx, delay); e != nil {
			return e
		}
		a.notBefore = time.Time{}
	}
	return fmt.Errorf("API request failed")
}

func (a *API) lookup(ctx context.Context, r Record) ([]DNSRecord, error) {
	matches := []DNSRecord{}
	for page := 1; page <= 100; page++ {
		q := url.Values{"name": {r.Name}, "type": {r.Type}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		var rows []DNSRecord
		if err := a.request(ctx, "GET", "/zones/"+a.zone+"/dns_records?"+q.Encode(), nil, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			if strings.EqualFold(strings.TrimSuffix(row.Name, "."), r.Name) && row.Type == r.Type {
				matches = append(matches, row)
			}
		}
		if len(matches) > 1 || len(rows) < 100 {
			return matches, nil
		}
	}
	return nil, fmt.Errorf("DNS lookup exceeded pagination limit")
}

func (a *API) update(ctx context.Context, id, ip string) error {
	return a.request(ctx, "PATCH", "/zones/"+a.zone+"/dns_records/"+url.PathEscape(id), map[string]string{"content": ip}, nil)
}
func (a *API) create(ctx context.Context, r Record, ip string) error {
	return a.request(ctx, "POST", "/zones/"+a.zone+"/dns_records", map[string]any{"name": r.Name, "type": r.Type, "content": ip, "ttl": r.TTL, "proxied": r.Proxied}, nil)
}

func discover(ctx context.Context, client *http.Client, s Source, family string) (string, error) {
	var b []byte
	var err error
	if s.File != "" {
		var f *os.File
		f, err = os.Open(s.File)
		if err == nil {
			b, err = io.ReadAll(io.LimitReader(f, 257))
			f.Close()
		}
	} else {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, "GET", s.URL, nil)
		if err == nil {
			var resp *http.Response
			resp, err = client.Do(req)
			if err == nil {
				if resp.StatusCode != 200 {
					resp.Body.Close()
					return "", fmt.Errorf("%s source HTTP %d", family, resp.StatusCode)
				}
				b, err = io.ReadAll(io.LimitReader(resp.Body, 257))
				resp.Body.Close()
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("%s discovery failed", family)
	}
	ip, e := netip.ParseAddr(strings.TrimSpace(string(b)))
	if len(b) > 256 || e != nil || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is4In6() || (family == "A") != ip.Is4() {
		return "", fmt.Errorf("%s source did not return a public %s address", family, family)
	}
	return ip.String(), nil
}
