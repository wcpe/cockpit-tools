package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// Priority:
//  1. X-Conversation-ID header / body.conversationId
//  2. History-prefix hash (messages excluding the last user turn)
//  3. First user message hash (single-turn start still sticks for follow-ups
//     that grow the history prefix)
func codebuddyConversationKeyFrom(clientHeader http.Header, payload map[string]any) string {
	var convID string
	if clientHeader != nil {
		convID = normalizeSessionKey(clientHeader.Get("X-Conversation-ID"))
	}
	if payload != nil && convID == "" {
		convID = normalizeSessionKey(stringValue(payload["conversationId"]))
		if convID == "" {
			if meta, ok := payload["metadata"].(map[string]any); ok {
				convID = normalizeSessionKey(stringValue(meta["conversation_id"]))
			}
		}
	}
	if convID != "" {
		return "conv:" + convID
	}
	if payload == nil {
		return ""
	}
	if h := promptHistoryHash(payload); h != "" {
		return "hist:" + h
	}
	return ""
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
		var text string
		switch c := m["content"].(type) {
		case string:
			text = c
		default:
			rawC, _ := json.Marshal(c)
			text = string(rawC)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		// Cap so huge first messages still hash cheaply but uniquely enough.
		if len(text) > 2048 {
			text = text[:2048]
		}
		sum := sha256.Sum256([]byte("wb2a:conv:" + text))
		return hex.EncodeToString(sum[:12])
	}
	return ""
}

// deriveStableRequestID produces a process-stable 32-hex id from a session key
// (aligned with workbuddy2api session.RequestIDForKey).
func deriveStableRequestID(sessionKey string) string {
	sum := sha256.Sum256([]byte("wb2a:req|" + sessionKey))
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
	for i, u := range order {
		if u == nil || u.ID != boundID {
			continue
		}
		if available, _ := gov.available(u.ID, model); !available {
			codebuddyAffinity.Unbind(sessionKey)
			return order, ""
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
