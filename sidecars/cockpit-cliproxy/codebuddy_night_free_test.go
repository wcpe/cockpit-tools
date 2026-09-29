package main

import (
	"testing"
	"time"
)

func TestNightFreeWindowWrap(t *testing.T) {
	p := codebuddyNightFreePolicy{
		Enabled:   true,
		Models:    []string{"hy3", "deepseek-v4.1-flash"},
		StartHour: 23,
		EndHour:   8,
	}
	// 23:30 CST = 15:30 UTC
	night := time.Date(2026, 9, 18, 15, 30, 0, 0, time.UTC)
	if !p.InNightWindow(night) {
		t.Fatal("23:30 CST should be in night window")
	}
	// 07:59 CST = 23:59 UTC previous day
	early := time.Date(2026, 9, 17, 23, 59, 0, 0, time.UTC)
	if !p.InNightWindow(early) {
		t.Fatal("07:59 CST should be in night window")
	}
	// 08:00 CST = 00:00 UTC
	out := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	if p.InNightWindow(out) {
		t.Fatal("08:00 CST should be outside window")
	}
	// 12:00 CST = 04:00 UTC
	noon := time.Date(2026, 9, 18, 4, 0, 0, 0, time.UTC)
	if p.InNightWindow(noon) {
		t.Fatal("12:00 CST should be outside window")
	}
}

func TestNightFreeAllowModel(t *testing.T) {
	p := codebuddyNightFreePolicy{
		Enabled:   true,
		Models:    []string{"hy3"},
		StartHour: 23,
		EndHour:   8,
	}
	night := time.Date(2026, 9, 18, 15, 30, 0, 0, time.UTC) // 23:30 CST
	day := time.Date(2026, 9, 18, 4, 0, 0, 0, time.UTC)     // 12:00 CST
	if ok, _ := p.AllowModel("hy3", night); !ok {
		t.Fatal("hy3 should be allowed at night")
	}
	if ok, reason := p.AllowModel("hy3", day); ok || reason == "" {
		t.Fatalf("hy3 should be denied by day, ok=%v reason=%q", ok, reason)
	}
	if ok, _ := p.AllowModel("glm-5.2", day); !ok {
		t.Fatal("glm should always be allowed")
	}
	// disabled policy → always allow
	p2 := codebuddyNightFreePolicy{Enabled: false, Models: []string{"hy3"}}
	if ok, _ := p2.AllowModel("hy3", day); !ok {
		t.Fatal("policy off should allow")
	}
}

func TestNightFreeFilterCatalog(t *testing.T) {
	p := codebuddyNightFreePolicy{Enabled: true, Models: []string{"hy3"}, StartHour: 23, EndHour: 8}
	day := time.Date(2026, 9, 18, 4, 0, 0, 0, time.UTC)
	ids := []string{"hy3", "glm-5.2"}
	out := p.FilterCatalogModels(ids, day)
	if len(out) != 1 || out[0] != "glm-5.2" {
		t.Fatalf("filtered=%v", out)
	}
	night := time.Date(2026, 9, 18, 15, 30, 0, 0, time.UTC)
	if got := p.FilterCatalogModels(ids, night); len(got) != 2 {
		t.Fatalf("night filter=%v", got)
	}
}

func TestSessionStickyReordersHead(t *testing.T) {
	// Multi-session spread: different keys pick via pool; same key sticky.
	codebuddyAffinity = &codebuddySessionAffinity{binds: map[string]codebuddySessionBind{}}
	a := &codebuddyUpstreamSpec{ID: "a1", BaseURL: "https://copilot.tencent.com"}
	b := &codebuddyUpstreamSpec{ID: "b2", BaseURL: "https://copilot.tencent.com"}
	gov := codebuddyGovernanceInit()
	codebuddyAffinity.Bind("conv:s1", "b2")
	order, sticky := pickUpstreamForSession([]*codebuddyUpstreamSpec{a, b}, "conv:s1", gov, "hy3")
	if sticky != "b2" || order[0].ID != "b2" {
		t.Fatalf("sticky=%q order0=%v", sticky, order[0].ID)
	}
}
