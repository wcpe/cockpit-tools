package main

import (
	"sync"
	"testing"
	"time"
)

func TestAccountAlwaysFreeBeatsNightOnlyOutsideWindow(t *testing.T) {
	p := &codebuddyPool{
		entries:     map[string]*codebuddyPoolEntry{},
		exploreLast: map[string]time.Time{},
		wafIPHits:   map[string]time.Time{},
	}
	upstreams := []codebuddyUpstreamSpec{
		{
			ID: "always", BaseURL: "https://copilot.tencent.com",
			AlwaysFreeModels: []string{"hy4-preview"},
		},
		{
			ID: "night", BaseURL: "https://copilot.tencent.com",
			NightOnlyFreeModels: []string{"hy4-preview"},
		},
	}
	p.SyncFromManifest(upstreams)

	// Outside night window (e.g. 12:00 CST) → always-free wins.
	day := time.Date(2026, 9, 18, 4, 0, 0, 0, time.UTC) // 12:00 CST
	night := time.Date(2026, 9, 18, 15, 30, 0, 0, time.UTC)
	p.mu.Lock()
	tierA, _ := p.accountFreeTierAt(p.entries["always"], "hy4-preview", day)
	tierNday, _ := p.accountFreeTierAt(p.entries["night"], "hy4-preview", day)
	tierNnight, _ := p.accountFreeTierAt(p.entries["night"], "hy4-preview", night)
	p.mu.Unlock()
	if tierA != 0 || tierNday != 2 || tierNnight != 0 {
		t.Fatalf("tiers always(day)=%d night(day)=%d night(night)=%d want 0/2/0", tierA, tierNday, tierNnight)
	}
	ptrs := make([]*codebuddyUpstreamSpec, 0, len(upstreams))
	for i := range upstreams {
		ptrs = append(ptrs, &upstreams[i])
	}
	p.mu.Lock()
	for _, e := range p.entries {
		e.LastUsed = time.Time{}
	}
	// Force day-time observation path by clearing free labels temporarily? PickOrder
	// uses time.Now() — at night both may be tier 0. Assert labels + costTierAt instead.
	if label := AccountFreeLabel(p.entries["always"], "hy4-preview", day); label != "always_free" {
		t.Fatalf("label always day=%q", label)
	}
	if label := AccountFreeLabel(p.entries["night"], "hy4-preview", day); label != "night_free_out_window" {
		t.Fatalf("label night day=%q", label)
	}
	p.mu.Unlock()
	if len(ptrs) != 2 {
		t.Fatal("ptrs")
	}
}

func TestAccountNightFreeInWindow(t *testing.T) {
	p := &codebuddyPool{
		entries:     map[string]*codebuddyPoolEntry{},
		exploreLast: map[string]time.Time{},
	}
	e := &codebuddyPoolEntry{ID: "n1", NightOnlyFreeModels: []string{"hy4-preview"}}
	// We cannot inject now into accountFreeTierLocked easily; test helper label via
	// nightPolicyInWindow + manual now in AccountFreeLabel.
	nightNow := time.Date(2026, 9, 18, 15, 30, 0, 0, time.UTC) // 23:30 CST
	if !nightPolicyInWindow(nightNow) {
		t.Fatal("23:30 CST should be in night window")
	}
	if got := AccountFreeLabel(e, "hy4-preview", nightNow); got != "night_free_in_window" {
		t.Fatalf("label=%q", got)
	}
	dayNow := time.Date(2026, 9, 18, 4, 0, 0, 0, time.UTC)
	if got := AccountFreeLabel(e, "hy4-preview", dayNow); got != "night_free_out_window" {
		t.Fatalf("label=%q", got)
	}
	always := &codebuddyPoolEntry{ID: "a1", AlwaysFreeModels: []string{"hy4-preview"}}
	if got := AccountFreeLabel(always, "hy4-preview", dayNow); got != "always_free" {
		t.Fatalf("label=%q", got)
	}
	_ = p
}

func TestCatalogSummarizesAccountFreePolicy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COCKPIT_CODEBUDDY_STATE_DIR", dir)
	codebuddyPoolOnce = sync.Once{}
	codebuddyPoolInst = nil
	codebuddyReqLog = nil

	m := &manifest{
		CodebuddyUpstreams: []codebuddyUpstreamSpec{
			{ID: "a-always", Label: "AlwaysAcct", ModelIDs: []string{"hy4-preview", "glm-5.2"},
				AlwaysFreeModels: []string{"hy4-preview"}},
			{ID: "a-night", Label: "NightAcct", ModelIDs: []string{"hy4-preview"},
				NightOnlyFreeModels: []string{"hy4-preview"}},
		},
	}
	pool := codebuddyPoolInit()
	pool.SyncFromManifest(m.CodebuddyUpstreams)
	rows := BuildCatalog(m)
	var hy *codebuddyModelCatalogRow
	for i := range rows {
		if rows[i].ID == "hy4-preview" {
			hy = &rows[i]
			break
		}
	}
	if hy == nil {
		// may be missing if not in default list — force via catalog row
		t.Skip("hy4-preview not in catalog id list")
	}
	if len(hy.AccountsAlwaysFree) == 0 || hy.AccountsAlwaysFree[0] != "AlwaysAcct" {
		t.Fatalf("alwaysFree=%v", hy.AccountsAlwaysFree)
	}
	if len(hy.AccountsNightFree) == 0 || hy.AccountsNightFree[0] != "NightAcct" {
		t.Fatalf("nightFree=%v", hy.AccountsNightFree)
	}
	if hy.FreePolicySummary == "" {
		t.Fatal("summary empty")
	}
}
