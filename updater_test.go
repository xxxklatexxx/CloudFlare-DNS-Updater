package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	mu                          sync.Mutex
	ip, ip6                     string
	records                     map[string][]DNSRecord
	calls, patches, discoveries int
	status                      int
	failName                    string
	throttle                    int
	retryAfter                  string
	delays                      []time.Duration
	bodies                      []map[string]any
	server                      *httptest.Server
}

func setup(t *testing.T) (*fixture, *Updater) {
	t.Helper()
	f := &fixture{ip: "203.0.113.2", ip6: "2001:db8::2", records: map[string][]DNSRecord{"home.example.com": {{ID: "id1", Name: "home.example.com", Type: "A", Content: "203.0.113.1"}}}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/ip" || r.URL.Path == "/ip6" {
			f.discoveries++
			if r.URL.Path == "/ip" {
				fmt.Fprint(w, f.ip)
			} else {
				fmt.Fprint(w, f.ip6)
			}
			return
		}
		f.calls++
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("missing Bearer token")
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			fmt.Fprint(w, "secret response test-secret")
			return
		}
		if f.throttle > 0 {
			f.throttle--
			value := f.retryAfter
			if value == "" {
				value = "2"
			}
			w.Header().Set("Retry-After", value)
			w.WriteHeader(429)
			return
		}
		if r.Method == "GET" {
			name := r.URL.Query().Get("name")
			if r.URL.Query().Get("type") == "" {
				t.Error("missing exact type query")
			}
			if name == f.failName {
				w.WriteHeader(403)
				return
			}
			rows := f.records[name]
			if rows == nil {
				rows = []DNSRecord{}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": rows})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		f.bodies = append(f.bodies, body)
		if r.Method == "PATCH" {
			f.patches++
			if len(body) != 1 || body["content"] == nil {
				t.Error("PATCH must contain only content")
			}
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			for name, rows := range f.records {
				for i := range rows {
					if rows[i].ID == id {
						rows[i].Content = body["content"].(string)
					}
				}
				f.records[name] = rows
			}
		} else if r.Method == "POST" {
			name := body["name"].(string)
			f.records[name] = []DNSRecord{{ID: "created", Name: name, Type: body["type"].(string), Content: body["content"].(string)}}
		} else {
			t.Errorf("unexpected method %s", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
	}))
	t.Cleanup(f.server.Close)
	c := Config{ZoneID: strings.Repeat("a", 32), Records: []Record{{Name: "home.example.com", Type: "A"}}, IPv4: Source{URL: f.server.URL + "/ip"}, Interval: "5m", Timeout: "1s", Username: "admin", Listen: ":8080", StateDir: t.TempDir(), TokenFile: "unused", PasswordFile: "unused", APIBase: f.server.URL}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	u, err := newUpdater(c, "test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	u.api.wait = func(ctx context.Context, d time.Duration) error {
		f.mu.Lock()
		f.delays = append(f.delays, d)
		f.mu.Unlock()
		return ctx.Err()
	}
	return f, u
}
func check(t *testing.T, u *Updater, force bool) {
	t.Helper()
	if err := u.reconcile(context.Background(), force); err != nil {
		t.Fatal(err)
	}
}
func TestChangedUnchangedRestartAndForce(t *testing.T) {
	f, u := setup(t)
	check(t, u, false)
	if f.calls != 2 || f.patches != 1 {
		t.Fatalf("initial calls=%d patches=%d", f.calls, f.patches)
	}
	check(t, u, false)
	if f.calls != 2 {
		t.Fatal("unchanged scheduled cycle contacted Cloudflare")
	}
	restored, err := newUpdater(u.cfg, "test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	check(t, restored, false)
	if f.calls != 2 || len(restored.snapshot().History) != 1 {
		t.Fatal("restart did not retain applied state/history")
	}
	f.ip = "203.0.113.3"
	check(t, restored, false)
	if f.calls != 4 || f.patches != 2 {
		t.Fatal("IP change not applied")
	}
	before := f.discoveries
	check(t, restored, true)
	if f.calls != 6 || f.patches != 3 || f.discoveries != before+1 {
		t.Fatal("force must freshly discover and PATCH even unchanged")
	}
	if !restored.snapshot().History[2].Forced {
		t.Fatal("force history missing")
	}
}
func TestInitialMatchingIPAndTargetChanges(t *testing.T) {
	f, u := setup(t)
	f.records["home.example.com"][0].Content = f.ip
	check(t, u, false)
	check(t, u, false)
	if f.calls != 1 || f.patches != 0 {
		t.Fatal("first matching record should initialize state then skip API")
	}
	c := u.cfg
	c.ZoneID = strings.Repeat("b", 32)
	v, err := newUpdater(c, "test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	check(t, v, false)
	if f.calls != 2 {
		t.Fatal("zone change must invalidate target state")
	}
	c.Records = []Record{{Name: "other.example.com", Type: "A"}}
	f.records["other.example.com"] = []DNSRecord{{ID: "id2", Name: "other.example.com", Type: "A", Content: f.ip}}
	v, err = newUpdater(c, "test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	check(t, v, false)
	if f.calls != 3 {
		t.Fatal("hostname change must invalidate state")
	}
}
func TestPartialFailureRetriesOnlyPending(t *testing.T) {
	f, u := setup(t)
	u.cfg.Records = append(u.cfg.Records, Record{Name: "vpn.example.com", Type: "A"})
	f.records["vpn.example.com"] = []DNSRecord{{ID: "id2", Name: "vpn.example.com", Type: "A", Content: "203.0.113.1"}}
	f.failName = "vpn.example.com"
	if u.reconcile(context.Background(), false) == nil {
		t.Fatal("expected partial failure")
	}
	if f.calls != 3 {
		t.Fatal(f.calls)
	}
	restored, err := newUpdater(u.cfg, "test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	f.failName = ""
	check(t, restored, false)
	if f.calls != 5 || f.patches != 2 {
		t.Fatal("only failed target should retry after restart")
	}
	check(t, restored, false)
	if f.calls != 5 {
		t.Fatal("successful targets should now skip API")
	}
}
func TestMissingDuplicateCredentialsAndDryRun(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "credentials", "dry-run"} {
		t.Run(mode, func(t *testing.T) {
			f, u := setup(t)
			switch mode {
			case "missing":
				delete(f.records, "home.example.com")
			case "duplicate":
				f.records["home.example.com"] = append(f.records["home.example.com"], f.records["home.example.com"][0])
			case "credentials":
				f.status = 401
			case "dry-run":
				u.dry = true
			}
			err := u.reconcile(context.Background(), false)
			if mode == "dry-run" {
				if err != nil || u.snapshot().Results[0].Result != "dry-run update" {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected failure")
			}
			if f.patches != 0 || len(u.applied) != 0 {
				t.Fatal("failed or dry-run cycle must not persist success")
			}
			b, _ := json.Marshal(u.snapshot())
			if strings.Contains(string(b), "test-secret") {
				t.Fatal("secret exposed")
			}
		})
	}
}
func TestExplicitCreation(t *testing.T) {
	f, u := setup(t)
	delete(f.records, "home.example.com")
	u.cfg.Records[0].Create = true
	check(t, u, false)
	if f.calls != 2 || u.snapshot().Results[0].Result != "created" {
		t.Fatal("opt-in creation failed")
	}
	check(t, u, false)
	if f.calls != 2 {
		t.Fatal("created state not retained")
	}
}
func TestAbsentIPv6DoesNotBlockA(t *testing.T) {
	f, u := setup(t)
	u.cfg.Records = append(u.cfg.Records, Record{Name: "home.example.com", Type: "AAAA"})
	u.cfg.IPv6 = Source{URL: f.server.URL + "/ip6"}
	f.ip6 = ""
	if u.reconcile(context.Background(), false) == nil {
		t.Fatal("absent IPv6 must be reported")
	}
	if f.patches != 1 || len(u.applied) != 1 || u.snapshot().Results[1].Result != "failed" {
		t.Fatal("A must succeed independently")
	}
}
func TestRateLimitsAndBoundedRetry(t *testing.T) {
	f, u := setup(t)
	f.throttle = 2
	check(t, u, false)
	if f.calls != 4 || len(f.delays) != 2 || f.delays[0] != 2*time.Second {
		t.Fatal("throttle retries should honor Retry-After")
	}
	f.status = 503
	if u.reconcile(context.Background(), true) == nil {
		t.Fatal("expected failure")
	}
	if f.calls != 7 {
		t.Fatal("must stop after three attempts")
	}
	f.status = 403
	if u.reconcile(context.Background(), true) == nil {
		t.Fatal("expected failure")
	}
	if f.calls != 8 {
		t.Fatal("must not retry credentials failure")
	}
}
func TestInvalidDiscovery(t *testing.T) {
	for _, ip := range []string{"", "127.0.0.1", "192.168.1.3", "::1", "2001:db8::2", "::ffff:8.8.8.8", "garbage"} {
		t.Run(ip, func(t *testing.T) {
			f, u := setup(t)
			f.ip = ip
			if u.reconcile(context.Background(), false) == nil {
				t.Fatal("expected invalid address")
			}
			if f.calls != 0 {
				t.Fatal("invalid IP must not reach Cloudflare")
			}
		})
	}
}
func TestFileIPv6AndCanonicalComparison(t *testing.T) {
	f, u := setup(t)
	p := filepath.Join(t.TempDir(), "ip6")
	os.WriteFile(p, []byte("2001:db8:0:0:0:0:0:2\n"), 0600)
	u.cfg.Records = []Record{{Name: "six.example.com", Type: "AAAA"}}
	u.cfg.IPv6 = Source{File: p}
	f.records["six.example.com"] = []DNSRecord{{ID: "id6", Name: "six.example.com", Type: "AAAA", Content: "2001:db8::2"}}
	check(t, u, false)
	if f.calls != 1 || f.patches != 0 {
		t.Fatal("equivalent IPv6 should not patch")
	}
	check(t, u, false)
	if f.calls != 1 {
		t.Fatal("persisted canonical IPv6 should skip API")
	}
}
func TestHistoryCorruptionAndPersistenceFailure(t *testing.T) {
	_, u := setup(t)
	os.WriteFile(filepath.Join(u.cfg.StateDir, "state.json"), []byte("broken"), 0600)
	if _, err := newUpdater(u.cfg, "test-secret", false); err == nil {
		t.Fatal("corrupt state should fail closed")
	}
	os.Mkdir(filepath.Join(u.cfg.StateDir, "state.json.tmp"), 0700)
	if u.reconcile(context.Background(), false) == nil {
		t.Fatal("state persistence failure must be reported")
	}
	if len(u.applied) != 0 || !u.snapshot().LastSuccess.IsZero() {
		t.Fatal("persistence failure must stay pending")
	}
}
func TestSerialization(t *testing.T) {
	_, u := setup(t)
	if !u.begin(true) {
		t.Fatal("first begin")
	}
	if u.begin(false) || u.begin(true) {
		t.Fatal("concurrent cycle allowed")
	}
	if u.reconcile(context.Background(), false) == nil {
		t.Fatal("concurrent reconcile allowed")
	}
	if err := u.finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	check(t, u, false)
}
