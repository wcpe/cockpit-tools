package main

import (
	"sync"
	"testing"
)

func TestBuildCatalogMergesCostAndCredits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COCKPIT_CODEBUDDY_STATE_DIR", dir)
	codebuddyPoolOnce = sync.Once{}
	codebuddyPoolInst = nil
	codebuddyReqLog = nil
	codebuddyModelsDevOnce = sync.Once{}
	codebuddyModelsDevInst = nil

	m := &manifest{
		ModelCredits: map[string]string{"hy3": "x0.00", "glm-5.2": "x0.05"},
		ModelCatalog: []map[string]any{
			{
				"id": "glm-5.2", "name": "GLM-5.2",
				"contextLength": float64(1000000), "maxOutputTokens": float64(131072),
				"credits": "x0.05", "efforts": []any{"low", "high"},
			},
		},
		DisabledModels: []string{"kimi-k2.8-preview"},
		CodebuddyUpstreams: []codebuddyUpstreamSpec{
			{ID: "a1", ModelIDs: []string{"hy3", "glm-5.2", "kimi-k2.8-preview"}},
		},
	}
	pool := codebuddyPoolInit()
	pool.SyncFromManifest(m.CodebuddyUpstreams)
	pool.NoteModelCost("a1", "hy3", 0, 1000)
	logs := codebuddyRequestLogInit()
	logs.Append(codebuddyRequestRecord{
		AccountID: "a1", Model: "hy3", Outcome: "ok", TotalTokens: 10,
		APIKeyID: "ck1", APIKeyLabel: "VSCode",
	})

	rows := BuildCatalog(m)
	byID := map[string]codebuddyModelCatalogRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	hy3, ok := byID["hy3"]
	if !ok {
		t.Fatal("hy3 missing from catalog")
	}
	if hy3.FreeObserved == nil || !*hy3.FreeObserved {
		t.Fatalf("hy3 should be free observed: %+v", hy3)
	}
	if byID["glm-5.2"].ContextLength != 1000000 {
		t.Fatalf("glm context=%d", byID["glm-5.2"].ContextLength)
	}
	if !byID["kimi-k2.8-preview"].Disabled {
		t.Fatal("kimi should be disabled")
	}
	if hy3.Requests < 1 {
		t.Fatalf("hy3 usage not merged: %+v", hy3)
	}

	recs, total := logs.ListPage(0, 10, "ck1")
	if total != 1 || len(recs) != 1 || recs[0].APIKeyLabel != "VSCode" {
		t.Fatalf("filter ck1 total=%d recs=%+v", total, recs)
	}
	stats := logs.StatsByAPIKey()
	if _, ok := stats["ck1"]; !ok {
		t.Fatalf("apiKey stats missing: %v", stats)
	}
	if cfg := CostExploreConfig(); cfg["enabled"] == nil {
		t.Fatal("cost explore config missing")
	}
}
