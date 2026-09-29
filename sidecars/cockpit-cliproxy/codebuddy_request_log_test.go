package main

import (
	"sync"
	"testing"
	"time"
)

func TestRequestLogAppendAndRing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COCKPIT_CODEBUDDY_STATE_DIR", dir)
	codebuddyReqLog = nil
	emitted := &capturingEmitter{}
	globalCodebuddyEmitter = emitted
	defer func() { globalCodebuddyEmitter = nil }()
	store := codebuddyRequestLogInit()
	for i := 0; i < codebuddyRequestLogMax+5; i++ {
		store.Append(codebuddyRequestRecord{
			AccountID: "a1",
			Model:     "glm-5.2",
			Outcome:   "ok",
		})
		emitCodebuddyUsageEvent(codebuddyRequestRecord{
			ID:        "evt-" + itoa(uint64(i)),
			AccountID: "a1",
			Model:     "glm-5.2",
			Outcome:   "ok",
		})
	}
	records := store.List(codebuddyRequestLogMax + 10)
	if len(records) != codebuddyRequestLogMax {
		t.Fatalf("len=%d want %d", len(records), codebuddyRequestLogMax)
	}
	if records[0].ID == "" || records[0].ID == records[1].ID {
		t.Fatalf("records not uniquely ordered: %q %q", records[0].ID, records[1].ID)
	}
	if len(emitted.events) != codebuddyRequestLogMax+5 {
		t.Fatalf("emitted=%d want %d", len(emitted.events), codebuddyRequestLogMax+5)
	}
	last := emitted.events[len(emitted.events)-1]
	if last["type"] != "codebuddy_usage" {
		t.Fatalf("event type = %v", last["type"])
	}
	if last["model"] != "glm-5.2" || last["outcome"] != "ok" {
		t.Fatalf("event payload = %#v", last)
	}
}

type capturingEmitter struct {
	mu     sync.Mutex
	events []map[string]any
}

func (c *capturingEmitter) emit(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := v.(map[string]any); ok {
		c.events = append(c.events, m)
	}
}

func (c *capturingEmitter) emitStartupStage(stage string) {
	c.emit(map[string]any{"type": "startup", "stage": stage})
}

func Test6004AutoCoolsOnlyThatModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COCKPIT_CODEBUDDY_STATE_DIR", dir)
	codebuddyGov = nil
	codebuddyReqLog = nil
	gov := codebuddyGovernanceInit()
	logs := codebuddyRequestLogInit()

	loc := time.FixedZone("UTC+8", 8*3600)
	future := time.Now().Add(45 * time.Minute).In(loc)
	body := `{"code":6004,"msg":"将在 ` + future.Format("2006-01-02 15:04:05") + ` UTC+8 重置"}`
	if !isModelRateLimit(body) {
		t.Fatal("6004 should classify as model rate limit")
	}
	parsed := parseRateResetAt(body)
	if parsed.IsZero() {
		t.Fatal("reset time should parse")
	}
	if !parsed.After(time.Now()) {
		t.Fatalf("parsed reset should be future: %v", parsed)
	}

	gov.coolSoftModel("acct1", "glm-5.3", parsed, "6004 model rate limit")
	logs.Append(codebuddyRequestRecord{
		AccountID:  "acct1",
		Model:      "glm-5.3",
		Outcome:    "rate_limited",
		ReasonCode: "6004",
		ResetAt:    formatOptionalTime(parsed),
	})

	if ok, reason := gov.available("acct1", "glm-5.3"); ok {
		t.Fatalf("glm-5.3 should auto-cool (reason=%q)", reason)
	}
	if ok, _ := gov.available("acct1", "hy3"); !ok {
		t.Fatal("other models must stay available")
	}
	ledger := gov.rateLimitedModels("acct1")
	if len(ledger) != 1 || ledger[0]["model"] != "glm-5.3" {
		t.Fatalf("rate_limited_models=%v", ledger)
	}
	records := logs.List(1)
	if len(records) != 1 || records[0].ReasonCode != "6004" {
		t.Fatalf("request log missing 6004: %+v", records)
	}
}
