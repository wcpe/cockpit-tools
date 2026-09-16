package main

import "testing"

func TestConversationKeyStableAcrossTurns(t *testing.T) {
	turn1 := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "什么是缓存命中？"},
		},
	}
	turn2 := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "什么是缓存命中？"},
			map[string]any{"role": "assistant", "content": "缓存命中指…"},
			map[string]any{"role": "user", "content": "再举个例子"},
		},
	}
	k1 := codebuddyConversationKeyFrom(nil, turn1)
	k2 := codebuddyConversationKeyFrom(nil, turn2)
	if k1 == "" || k2 == "" {
		t.Fatalf("keys empty: %q %q", k1, k2)
	}
	if k1 != k2 {
		t.Fatalf("same conversation must share key: %q vs %q", k1, k2)
	}
	other := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "另一个话题"},
		},
	}
	if codebuddyConversationKeyFrom(nil, other) == k1 {
		t.Fatal("different conversations must not share key")
	}
}

func TestStickyPickMovesBoundAccountFirst(t *testing.T) {
	t.Setenv("COCKPIT_CODEBUDDY_STATE_DIR", t.TempDir())
	codebuddyGov = nil
	gov := codebuddyGovernanceInit()
	codebuddyAffinity.Unbind("conv:x")
	a := &codebuddyUpstreamSpec{ID: "a", AccessToken: "1", BaseURL: "https://x"}
	b := &codebuddyUpstreamSpec{ID: "b", AccessToken: "2", BaseURL: "https://x"}
	codebuddyAffinity.Bind("conv:x", "b")
	order, sticky := pickUpstreamForSession([]*codebuddyUpstreamSpec{a, b}, "conv:x", gov, "glm")
	if sticky != "b" || order[0].ID != "b" {
		t.Fatalf("sticky=%q order=%v", sticky, []string{order[0].ID, order[1].ID})
	}
}
