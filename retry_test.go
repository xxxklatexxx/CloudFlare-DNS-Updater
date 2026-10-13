package main

import (
	"context"
	"testing"
	"time"
)

func TestLongRateLimitDefersAllAPIWork(t *testing.T) {
	f, u := setup(t)
	f.throttle = 1
	f.retryAfter = "90"
	if u.reconcile(context.Background(), false) == nil {
		t.Fatal("expected deferred check")
	}
	if f.calls != 1 || len(f.delays) != 0 {
		t.Fatal("long cooldown must not sleep or retry early")
	}
	if u.reconcile(context.Background(), true) == nil {
		t.Fatal("force must respect server cooldown")
	}
	if f.calls != 1 {
		t.Fatal("cooldown should prevent API calls across cycles")
	}
	u.api.notBefore = time.Time{} // simulate cooldown expiry without waiting
	check(t, u, false)
	if f.calls != 3 {
		t.Fatal("pending target did not retry after cooldown")
	}
}

func TestCreationDoesNotRetryUncertainResponses(t *testing.T) {
	f, u := setup(t)
	f.status = 503
	if u.api.create(context.Background(), u.cfg.Records[0], f.ip) == nil {
		t.Fatal("expected failed creation")
	}
	if f.calls != 1 || len(f.delays) != 0 {
		t.Fatal("creation must not retry uncertain 5xx response")
	}
}

func TestRemovedTargetStateIsPruned(t *testing.T) {
	f, u := setup(t)
	check(t, u, false)
	c := u.cfg
	c.Records = []Record{{Name: "new.example.com", Type: "A"}}
	v, err := newUpdater(c, "test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.applied) != 0 {
		t.Fatal("removed target state retained")
	}
	f.records["new.example.com"] = []DNSRecord{{ID: "id2", Name: "new.example.com", Type: "A", Content: f.ip}}
	check(t, v, false)
	restored, err := newUpdater(u.cfg, "test-secret", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.applied) != 0 {
		t.Fatal("re-added target must initialize fresh")
	}
	check(t, restored, false)
	if f.calls != 4 {
		t.Fatal("re-added target did not contact API")
	}
}
