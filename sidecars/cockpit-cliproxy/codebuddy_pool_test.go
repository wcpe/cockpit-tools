package main

import (
	"testing"
	"time"
)

func TestCodebuddyPoolPrefersFreeAccount(t *testing.T) {
	p := &codebuddyPool{
		entries:     map[string]*codebuddyPoolEntry{},
		exploreLast: map[string]time.Time{},
		wafIPHits:   map[string]time.Time{},
	}
	creditsFree := int64(1)
	creditsPaid := int64(1_000_000)
	upstreams := []*codebuddyUpstreamSpec{
		{ID: "free", BaseURL: "https://copilot.tencent.com", Credits: &creditsFree},
		{ID: "paid", BaseURL: "https://copilot.tencent.com", Credits: &creditsPaid},
	}
	p.SyncFromPointers(upstreams)
	p.NoteModelCost("free", "hy4-preview", 0, 1000)
	p.NoteModelCost("paid", "hy4-preview", 2.9, 1000)
	for i := 0; i < 30; i++ {
		// Reset lastUsed gap so anti-herd does not block.
		p.mu.Lock()
		p.entries["free"].LastUsed = time.Time{}
		p.entries["paid"].LastUsed = time.Time{}
		p.mu.Unlock()
		order := p.PickOrder(upstreams, "hy4-preview", "", "")
		if len(order) == 0 || order[0].ID != "free" {
			t.Fatalf("pick %d head=%v want free", i, order)
		}
	}
}

func TestCodebuddyPoolRealmPrefixFilters(t *testing.T) {
	p := &codebuddyPool{
		entries:     map[string]*codebuddyPoolEntry{},
		exploreLast: map[string]time.Time{},
		wafIPHits:   map[string]time.Time{},
	}
	upstreams := []*codebuddyUpstreamSpec{
		{ID: "cn1", BaseURL: "https://copilot.tencent.com"},
		{ID: "g1", BaseURL: "https://www.workbuddy.ai"},
	}
	p.SyncFromPointers(upstreams)
	order := p.PickOrder(upstreams, "global:glm-5.2", "", "")
	if len(order) == 0 || order[0].ID != "g1" {
		t.Fatalf("global prefix order=%v want g1 first", order)
	}
}

func TestCodebuddyPoolWafIPTripAtTwoAccounts(t *testing.T) {
	p := &codebuddyPool{
		entries:     map[string]*codebuddyPoolEntry{},
		exploreLast: map[string]time.Time{},
		wafIPHits:   map[string]time.Time{},
	}
	if p.NoteWaf("a1") {
		t.Fatal("single waf hit should not trip IP gate")
	}
	if !p.NoteWaf("a2") {
		t.Fatal("second distinct account should trip IP gate")
	}
	if !p.WafIPActive() {
		t.Fatal("waf ip should be active")
	}
}

func TestCodebuddyPoolNoteModelCostFreeTierEnd(t *testing.T) {
	p := &codebuddyPool{
		entries:     map[string]*codebuddyPoolEntry{},
		exploreLast: map[string]time.Time{},
	}
	p.NoteModelCost("u1", "m", 0, 1000)
	p.mu.Lock()
	tier, _ := p.costTierLocked(p.entries["u1"], "m")
	p.mu.Unlock()
	if tier != 0 {
		t.Fatalf("tier=%d want 0 free", tier)
	}
	p.NoteModelCost("u1", "m", 2.0, 1000)
	p.mu.Lock()
	tier, cost := p.costTierLocked(p.entries["u1"], "m")
	p.mu.Unlock()
	if tier != 2 || cost <= 0 {
		t.Fatalf("tier=%d cost=%v want paid", tier, cost)
	}
}

func TestCodebuddyLooksWafBlocked(t *testing.T) {
	if !codebuddyLooksWafBlocked(403, "<html>blocked</html>") {
		t.Fatal("html 403 should be waf")
	}
	if codebuddyLooksWafBlocked(403, `{"code":1,"msg":"forbidden"}`) {
		t.Fatal("business envelope should not be waf")
	}
	if !codebuddyLooksWafBlocked(403, "") {
		t.Fatal("empty 403 should be waf")
	}
}

func TestCodebuddySplitRealmModel(t *testing.T) {
	r, m := codebuddySplitRealmModel("cn:hy3")
	if r != "cn" || m != "hy3" {
		t.Fatalf("got %q %q", r, m)
	}
	r, m = codebuddySplitRealmModel("hy3")
	if r != "" || m != "hy3" {
		t.Fatalf("got %q %q", r, m)
	}
}
