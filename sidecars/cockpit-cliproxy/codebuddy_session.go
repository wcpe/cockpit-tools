package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// codebuddy_session.go — conversation sticky routing for the CodeBuddy gateway.
//
// Prompt cache is per-account. Round-robin across accounts invalidates the
// upstream cache prefix and burns credits. Bind a conversation to one account
// and keep using it while that account stays healthy for the requested model.
//
// Most OpenAI-compatible clients do NOT send X-Conversation-ID. Per-turn
// X-Conversation-Request-ID is intentionally ignored (it changes every request).
// Fallback: hash the message-history prefix so multi-turn chats share a key.

const codebuddySessionAffinityTTL = 30 * time.Minute

type codebuddySessionBind struct {
	accountID  string
	lastActive time.Time
}

type codebuddySessionAffinity struct {
	mu    sync.RWMutex
	binds map[string]codebuddySessionBind
}

var codebuddyAffinity = &codebuddySessionAffinity{
	binds: map[string]codebuddySessionBind{},
}

func normalizeSessionKey(s string) string {
	return strings.Trim(strings.TrimSpace(s), "\"")
}

// codebuddyConversationKeyFrom extracts a stable conversation key.
// Priority (aligned with workbuddy2api session.ExtractKey + sticky fallback):
//  1. X-Conversation-ID header / body.conversation_id / body.conversationId
//  2. body.prompt_cache_key (OpenAI prefix-cache / pi-ai clients)
//  3. History-prefix hash (messages excluding the last user turn)
//  4. First user message signature (StickyFallbackKey semantics; suppressed
//     when metadata.user_id / user_id is present — anti-monopoly)
func codebuddyConversationKeyFrom(clientHeader http.Header, payload map[string]any) string {
	var convID string
	if clientHeader != nil {
		convID = normalizeSessionKey(clientHeader.Get("X-Conversation-ID"))
	}
	if payload != nil && convID == "" {
		if meta, ok := payload["metadata"].(map[string]any); ok {
			convID = normalizeSessionKey(stringValue(meta["conversation_id"]))
			if convID == "" {
				convID = normalizeSessionKey(stringValue(meta["conversationId"]))
			}
		}
		if convID == "" {
			convID = normalizeSessionKey(stringValue(payload["conversation_id"]))
		}
		if convID == "" {
			convID = normalizeSessionKey(stringValue(payload["conversationId"]))
		}
	}
	if convID != "" {
		return "conv:" + convID
	}
	if payload == nil {
		return ""
	}
	// prompt_cache_key: OpenAI prefix-cache key, same conversation semantics.
	if pck := normalizeSessionKey(stringValue(payload["prompt_cache_key"])); pck != "" {
		return "pck:" + pck
	}
	if hasCodebuddyUserID(payload) {
		// user_id is too coarse for sticky (anti-monopoly); fall back to RR.
		return ""
	}
	if h := promptHistoryHash(payload); h != "" {
		return "hist:" + h
	}
	return ""
}

func hasCodebuddyUserID(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	if stringValue(payload["user_id"]) != "" || stringValue(payload["userId"]) != "" {
		return true
	}
	if meta, ok := payload["metadata"].(map[string]any); ok {
		if stringValue(meta["user_id"]) != "" || stringValue(meta["userId"]) != "" {
			return true
		}
	}
	return false
}

// contentSignature for sticky/history keys: text parts concatenated; non-text
// parts contribute [type:sha256/8] so pure-image turns still stick (workbuddy2api G1).
func codebuddyContentSignature(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		hasNonText := false
		for _, part := range c {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			if typ == "" || typ == "text" {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
				continue
			}
			hasNonText = true
			raw, _ := json.Marshal(m)
			sum := sha256.Sum256(raw)
			b.WriteString("\n[" + typ + ":" + hex.EncodeToString(sum[:4]) + "]\n")
		}
		out := b.String()
		if !hasNonText {
			return out
		}
		return strings.TrimSpace(out)
	case nil:
		return ""
	default:
		raw, _ := json.Marshal(c)
		return string(raw)
	}
}

// promptHistoryHash identifies a conversation by its first user message so
// multi-turn requests stay bound to the same account (cache prefix preserved).
func promptHistoryHash(payload map[string]any) string {
	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "user" {
			continue
		}
		text := strings.TrimSpace(codebuddyContentSignature(m["content"]))
		if text == "" {
			// First user message has no signature → do not walk further
			// (stable key for the whole session; avoid drift).
			return ""
		}
		if len(text) > 2048 {
			text = text[:2048]
		}
		sum := sha256.Sum256([]byte("wb2a:conv:" + text))
		return hex.EncodeToString(sum[:12])
	}
	return ""
}

// codebuddyTurnSignature keys the current user turn (last user message) for
// turn-level conversationRequestID (#170).
func codebuddyTurnSignature(payload map[string]any) string {
	msgs, ok := payload["messages"].([]any)
	if !ok {
		return ""
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m, ok := msgs[i].(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role != "user" {
			continue
		}
		sig := strings.TrimSpace(codebuddyContentSignature(m["content"]))
		if sig == "" {
			return ""
		}
		if len(sig) > 2048 {
			sig = sig[:2048]
		}
		return fmt.Sprintf("u%d:%s", i, sig)
	}
	return ""
}

// deriveStableRequestID produces a process-stable 32-hex id from a session key
// (aligned with workbuddy2api session.RequestIDForKey).
func deriveStableRequestID(sessionKey string) string {
	sum := sha256.Sum256([]byte("wb2a:req|" + sessionKey))
	return hex.EncodeToString(sum[:16])
}

// deriveTurnRequestID: session-scoped turn-level aggregation key (#170).
// Same turn text in different sessions must not collide → sessKey in the mix.
func deriveTurnRequestID(sessionKey, turnSig string) string {
	if turnSig == "" {
		if sessionKey != "" {
			return deriveStableRequestID(sessionKey)
		}
		return ""
	}
	key := turnSig
	if sessionKey != "" {
		key = sessionKey + ":" + turnSig
	}
	sum := sha256.Sum256([]byte("wb2a:turn|" + key))
	return hex.EncodeToString(sum[:16])
}

func (a *codebuddySessionAffinity) Get(key string) (string, bool) {
	if key == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bind, ok := a.binds[key]
	if !ok {
		return "", false
	}
	if time.Since(bind.lastActive) > codebuddySessionAffinityTTL {
		delete(a.binds, key)
		return "", false
	}
	bind.lastActive = time.Now()
	a.binds[key] = bind
	return bind.accountID, true
}

func (a *codebuddySessionAffinity) Bind(key, accountID string) {
	if key == "" || accountID == "" {
		return
	}
	a.mu.Lock()
	a.binds[key] = codebuddySessionBind{accountID: accountID, lastActive: time.Now()}
	a.mu.Unlock()
}

func (a *codebuddySessionAffinity) Unbind(key string) {
	if key == "" {
		return
	}
	a.mu.Lock()
	delete(a.binds, key)
	a.mu.Unlock()
}

func (a *codebuddySessionAffinity) UnbindAccount(accountID string) {
	if accountID == "" {
		return
	}
	a.mu.Lock()
	for key, bind := range a.binds {
		if bind.accountID == accountID {
			delete(a.binds, key)
		}
	}
	a.mu.Unlock()
}

// ClearAll drops every sticky binding (manual reset from UI).
func (a *codebuddySessionAffinity) ClearAll() int {
	a.mu.Lock()
	n := len(a.binds)
	a.binds = map[string]codebuddySessionBind{}
	a.mu.Unlock()
	return n
}

func (a *codebuddySessionAffinity) Snapshot() map[string]string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]string, len(a.binds))
	now := time.Now()
	for k, v := range a.binds {
		if now.Sub(v.lastActive) <= codebuddySessionAffinityTTL {
			out[k] = v.accountID
		}
	}
	return out
}

// pickUpstreamForSession reorders: sticky account first if available for model.
func pickUpstreamForSession(
	order []*codebuddyUpstreamSpec,
	sessionKey string,
	gov *codebuddyGovernance,
	model string,
) ([]*codebuddyUpstreamSpec, string) {
	if sessionKey == "" || len(order) == 0 {
		return order, ""
	}
	boundID, ok := codebuddyAffinity.Get(sessionKey)
	if !ok {
		return order, ""
	}
	pool := codebuddyPoolInit()
	for i, u := range order {
		if u == nil || u.ID != boundID {
			continue
		}
		if available, _ := gov.available(u.ID, model); !available {
			codebuddyAffinity.Unbind(sessionKey)
			return order, boundID // keep stickyID so caller can fail-fast
		}
		if available, _ := pool.Available(u.ID, model); !available {
			codebuddyAffinity.Unbind(sessionKey)
			return order, boundID
		}
		if i == 0 {
			return order, u.ID
		}
		reordered := make([]*codebuddyUpstreamSpec, 0, len(order))
		reordered = append(reordered, u)
		reordered = append(reordered, order[:i]...)
		reordered = append(reordered, order[i+1:]...)
		return reordered, u.ID
	}
	return order, ""
}
