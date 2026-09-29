package main

import (
	"testing"
	"time"
)

func TestDeriveAccountStableIDStableAndPurposeIsolated(t *testing.T) {
	a := deriveAccountStableID("u1", "machine")
	b := deriveAccountStableID("u1", "machine")
	if a != b {
		t.Fatalf("unstable: %q vs %q", a, b)
	}
	if len(a) != 36 {
		t.Fatalf("len=%d want 36", len(a))
	}
	if deriveAccountStableID("u1", "session") == a {
		t.Fatal("purpose should isolate ids")
	}
	if deriveAccountStableID("u2", "machine") == a {
		t.Fatal("uid should isolate ids")
	}
	if deriveAccountStableID("", "machine") != "" && deriveAccountStableID("", "machine") != deriveAccountStableID("", "machine") {
		t.Fatal("empty uid should still be deterministic")
	}
}

func TestIsModelRateLimit6004(t *testing.T) {
	if !isModelRateLimit(`{"code":6004,"msg":"x"}`) {
		t.Fatal("6004 should match")
	}
	if !isModelRateLimit(`{"code": 6004,"msg":"x"}`) {
		t.Fatal("spaced 6004 should match")
	}
	if isModelRateLimit(`{"code":11140,"msg":"x"}`) {
		t.Fatal("11140 should not be model rate limit")
	}
}

func TestParseRateResetAtUTC8(t *testing.T) {
	body := `{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`
	ts := parseRateResetAt(body)
	if ts.IsZero() {
		t.Fatal("expected parseable reset time")
	}
	want := time.Date(2026, 9, 11, 18, 33, 27, 0, time.FixedZone("UTC+8", 8*3600)).UTC()
	if !ts.Equal(want) {
		t.Fatalf("got %v want %v", ts, want)
	}
}

func TestGovernanceModelCooldownDoesNotBlockOtherModels(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COCKPIT_CODEBUDDY_STATE_DIR", dir)
	codebuddyGov = nil
	gov := codebuddyGovernanceInit()
	reset := time.Now().Add(30 * time.Minute)
	gov.coolSoftModel("acct1", "glm-5.3", reset, "6004")
	if ok, _ := gov.available("acct1", "glm-5.3"); ok {
		t.Fatal("glm-5.3 should be cooling")
	}
	if ok, _ := gov.available("acct1", "hy3"); !ok {
		t.Fatal("hy3 should remain available")
	}
	if ok, _ := gov.available("acct1", ""); !ok {
		t.Fatal("account-level should not be blocked by model-only cooldown")
	}
	ledger := gov.rateLimitedModels("acct1")
	if len(ledger) != 1 || ledger[0]["model"] != "glm-5.3" {
		t.Fatalf("ledger=%v", ledger)
	}
}

func TestSanitizeRewritesFingerprint(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude. 11128"
	out := sanitizeText(in)
	if out == in {
		t.Fatal("expected rewrite")
	}
	if !contains(out, "CLI tool for Claude") {
		t.Fatalf("identity not rewritten: %q", out)
	}
	if contains(out, "11128") {
		t.Fatalf("11128 should be rewritten: %q", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestInjectDeepSeekThinking(t *testing.T) {
	p := map[string]any{"model": "deepseek-v4.1-flash"}
	injectDeepSeekThinking(p, "deepseek-v4.1-flash")
	thinking, _ := p["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] != "enabled" {
		t.Fatalf("thinking not injected: %v", p)
	}
	if p["reasoning_effort"] != "high" {
		t.Fatalf("effort missing: %v", p)
	}
}

func TestParseRateResetEnglish(t *testing.T) {
	body := `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-09-18 16:06:52 UTC+8, alternatively, you can switch to the other models to continue using it."}`
	ts := parseRateResetAt(body)
	if ts.IsZero() {
		t.Fatal("english reset time should parse")
	}
	if ts.Year() != 2026 || ts.Month() != time.September || ts.Day() != 18 {
		t.Fatalf("parsed %v", ts)
	}
	// Natural language must not match.
	body2 := `{"code":6004,"msg":"usage will reset at the end of the day"}`
	if !parseRateResetAt(body2).IsZero() {
		t.Fatal("natural language should not parse")
	}
}

func TestHardQuotaNext0400(t *testing.T) {
	next := codebuddyNext0400CST()
	if next.Before(time.Now().UTC()) {
		t.Fatal("next 04:00 CST should be in the future")
	}
	if next.After(time.Now().UTC().Add(25 * time.Hour)) {
		t.Fatal("next 04:00 CST should be within a day")
	}
}

func TestApplySystemPromptModeAppendInsertsAfterLeadingBlock(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "client-a"},
			map[string]any{"role": "developer", "content": "client-b"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	applySystemPromptMode(payload, "append", false)
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d", len(msgs))
	}
	roles := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		roles = append(roles, mm["role"].(string))
	}
	want := []string{"system", "developer", "system", "user"}
	for i, r := range roles {
		if r != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}
	// Leading client systems stay verbatim.
	first, _ := msgs[0].(map[string]any)
	if first["content"] != "client-a" {
		t.Fatalf("client system rewritten: %v", first["content"])
	}
}

func TestApplySystemPromptModeAppendDegradedReplaces(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "fingerprint-bait"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	applySystemPromptMode(payload, "append", true)
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("degraded append should replace, messages = %#v", msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["content"] != codebuddyNeutralSystemPrompt {
		t.Fatalf("degraded content = %v", sys["content"])
	}
}

func TestEnsureCodebuddyUsageTotal(t *testing.T) {
	u := map[string]any{"prompt_tokens": float64(5), "completion_tokens": float64(3)}
	out := ensureCodebuddyUsageTotal(u)
	if out["total_tokens"] != float64(8) {
		t.Fatalf("total_tokens = %v", out["total_tokens"])
	}
	if _, ok := u["total_tokens"]; ok {
		t.Fatal("input map must not be mutated")
	}
	// Already has total → unchanged.
	u2 := map[string]any{"total_tokens": float64(9), "prompt_tokens": float64(5)}
	if ensureCodebuddyUsageTotal(u2)["total_tokens"] != float64(9) {
		t.Fatal("existing total should be preserved")
	}
}

func TestJsonLooksComplete(t *testing.T) {
	if !jsonLooksComplete(`{"city":"北京"}`) {
		t.Fatal("complete object should pass")
	}
	if jsonLooksComplete(`{"ci`) {
		t.Fatal("truncated object should fail")
	}
	if !jsonLooksComplete("") {
		t.Fatal("empty args (no-param tool) should pass")
	}
}
