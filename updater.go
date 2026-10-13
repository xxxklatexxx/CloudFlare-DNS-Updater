package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Entry struct {
	Timestamp time.Time `json:"timestamp"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	OldIP     string    `json:"old_ip"`
	NewIP     string    `json:"new_ip"`
	Result    string    `json:"result"`
	Error     string    `json:"error,omitempty"`
	Forced    bool      `json:"forced"`
}
type Status struct {
	Running     bool      `json:"running"`
	Mode        string    `json:"mode"`
	IPv4        string    `json:"ipv4"`
	IPv6        string    `json:"ipv6"`
	LastCheck   time.Time `json:"last_check"`
	LastSuccess time.Time `json:"last_success"`
	NextCheck   time.Time `json:"next_check"`
	Error       string    `json:"error,omitempty"`
	Records     []Record  `json:"records"`
	Results     []Entry   `json:"results"`
	History     []Entry   `json:"history"`
}
type DiskState struct {
	Applied map[string]string `json:"applied"`
	History []Entry           `json:"history"`
}
type Updater struct {
	cfg     Config
	api     *API
	dry     bool
	run     sync.Mutex
	mu      sync.Mutex
	status  Status
	history []Entry
	applied map[string]string
}

func newUpdater(c Config, token string, dry bool) (*Updater, error) {
	u := &Updater{cfg: c, dry: dry, api: &API{base: c.APIBase, zone: c.ZoneID, token: token, client: newClient(c.timeout), wait: waitContext}}
	u.status.Records = c.Records
	u.status.Results = []Entry{}
	u.history = []Entry{}
	u.applied = map[string]string{}
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(c.StateDir, "state.json"))
	if err == nil {
		var disk DiskState
		if err = json.Unmarshal(b, &disk); err != nil {
			return nil, fmt.Errorf("state is corrupt; restore or move state.json before starting")
		}
		u.history = disk.History
		if disk.Applied != nil {
			u.applied = disk.Applied
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if len(u.history) > 1000 {
		u.history = u.history[len(u.history)-1000:]
	}
	// Removed targets must not carry stale success into a later re-addition.
	valid := map[string]bool{}
	for _, r := range c.Records {
		valid[u.key(r)] = true
	}
	for key := range u.applied {
		if !valid[key] {
			delete(u.applied, key)
		}
	}
	return u, nil
}
func (u *Updater) snapshot() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	s.Results = append([]Entry{}, s.Results...)
	s.History = append([]Entry{}, u.history...)
	return s
}
func (u *Updater) key(r Record) string { return u.cfg.ZoneID + "/" + r.Name + "/" + r.Type }
func (u *Updater) save(r Record, e Entry) (err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	defer func() {
		if err != nil {
			delete(u.applied, u.key(r))
		}
	}()
	u.status.Results = append(u.status.Results, e)
	if !u.dry {
		if e.Result == "failed" {
			delete(u.applied, u.key(r))
		} else {
			u.applied[u.key(r)] = e.NewIP
		}
	}
	if e.Result != "unchanged" {
		u.history = append(u.history, e)
		if len(u.history) > 1000 {
			u.history = u.history[len(u.history)-1000:]
		}
	}
	b, err := json.MarshalIndent(DiskState{Applied: u.applied, History: u.history}, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(u.cfg.StateDir, "state.json")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		delete(u.applied, u.key(r))
	}
	return err
}
func (u *Updater) begin(force bool) bool {
	if !u.run.TryLock() {
		return false
	}
	u.mu.Lock()
	u.status.Running = true
	u.status.Results = []Entry{}
	u.status.Error = ""
	u.status.Mode = "scheduled"
	if force {
		u.status.Mode = "forced"
	}
	if u.dry {
		u.status.Mode += " (dry run)"
	}
	u.mu.Unlock()
	return true
}
func (u *Updater) reconcile(ctx context.Context, force bool) error {
	if !u.begin(force) {
		return fmt.Errorf("reconciliation already running")
	}
	return u.finish(ctx, force)
}

// Caller must own run via begin. Manual and scheduled cycles share the same lock.
func (u *Updater) finish(ctx context.Context, force bool) error {
	defer u.run.Unlock()
	var cycleErr error
	ips := map[string]string{}
	sourceErrors := map[string]error{}
	for _, r := range u.cfg.Records {
		if _, ok := ips[r.Type]; ok {
			continue
		}
		s := u.cfg.IPv4
		if r.Type == "AAAA" {
			s = u.cfg.IPv6
		}
		ip, err := discover(ctx, u.api.client, s, r.Type)
		ips[r.Type] = ip
		sourceErrors[r.Type] = err
	}
	u.mu.Lock()
	u.status.IPv4 = ips["A"]
	u.status.IPv6 = ips["AAAA"]
	u.mu.Unlock()
	for _, r := range u.cfg.Records {
		e := Entry{Timestamp: time.Now().UTC(), Name: r.Name, Type: r.Type, NewIP: ips[r.Type], Forced: force}
		err := sourceErrors[r.Type]
		u.mu.Lock()
		applied := u.applied[u.key(r)]
		u.mu.Unlock()
		if err == nil && !force && applied == e.NewIP {
			e.OldIP = applied
			e.Result = "unchanged"
		} else if err == nil {
			var rows []DNSRecord
			rows, err = u.api.lookup(ctx, r)
			if err == nil {
				switch len(rows) {
				case 0:
					if !r.Create {
						err = fmt.Errorf("record missing (creation disabled)")
					} else if u.dry {
						e.Result = "dry-run create"
					} else {
						err = u.api.create(ctx, r, e.NewIP)
						e.Result = "created"
					}
				case 1:
					e.OldIP = rows[0].Content
					old, parseErr := netip.ParseAddr(e.OldIP)
					target, _ := netip.ParseAddr(e.NewIP)
					if !force && parseErr == nil && old == target {
						e.Result = "unchanged"
					} else if u.dry {
						e.Result = "dry-run update"
					} else if rows[0].ID == "" {
						err = fmt.Errorf("Cloudflare record has no ID")
					} else {
						err = u.api.update(ctx, rows[0].ID, e.NewIP)
						e.Result = "updated"
					}
				default:
					err = fmt.Errorf("ambiguous record: multiple exact hostname/type matches")
				}
			}
		}
		if err != nil {
			e.Result = "failed"
			e.Error = err.Error()
			cycleErr = fmt.Errorf("one or more records failed")
		}
		if err = u.save(r, e); err != nil {
			cycleErr = fmt.Errorf("state persistence failed")
			u.mu.Lock()
			u.status.Error = cycleErr.Error()
			u.mu.Unlock()
		}
		log.Printf("record=%s type=%s result=%s old=%s new=%s error=%s", e.Name, e.Type, e.Result, e.OldIP, e.NewIP, e.Error)
	}
	u.mu.Lock()
	u.status.Running = false
	u.status.LastCheck = time.Now().UTC()
	if cycleErr == nil {
		u.status.LastSuccess = u.status.LastCheck
	}
	if cycleErr != nil {
		u.status.Error = cycleErr.Error()
	}
	u.mu.Unlock()
	return cycleErr
}
func (u *Updater) schedule(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		cycle, cancel := context.WithTimeout(ctx, cycleBudget(u.cfg))
		_ = u.reconcile(cycle, false)
		cancel()
		next := time.Now().Add(u.cfg.poll)
		u.mu.Lock()
		u.status.NextCheck = next
		u.mu.Unlock()
		if waitContext(ctx, time.Until(next)) != nil {
			return
		}
	}
}
