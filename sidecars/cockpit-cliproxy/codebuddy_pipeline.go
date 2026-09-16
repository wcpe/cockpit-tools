package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// codebuddy_pipeline.go ports workbuddy2api request-path enhancements:
// stable account device headers, conversation header family, DeepSeek thinking
// injection, system prompt passthrough/custom, and fingerprint sanitize.

// deriveAccountStableID: sha256("wb2a:"+purpose+":"+uid)[:36 hex].
// Account-scoped, restart-stable (fixed salt), mirrors hub wb_fingerprint.
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

func injectAccountStableHeaders(req *http.Request, uid string) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(uid, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(uid, "session"))
}

func randomHex32() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fallback: still deterministic enough for diagnostics.
		sum := sha256.Sum256([]byte(timeNowFallbackSeed()))
		return hex.EncodeToString(sum[:16])
	}
	return hex.EncodeToString(buf[:])
}

func timeNowFallbackSeed() string {
	return "codebuddy-fallback"
}

// applyConversationHeaders injects official client conversation header family.
// X-Conversation-Request-ID is the aggregation primary key (reused across
// rotation/retry). X-Conversation-ID is passthrough only.
func applyConversationHeaders(req *http.Request, clientReq *http.Request, clientBody map[string]any, conversationRequestID string) {
	if conversationRequestID == "" {
		if clientReq != nil {
			conversationRequestID = strings.TrimSpace(clientReq.Header.Get("X-Conversation-Request-ID"))
		}
		if conversationRequestID == "" {
			conversationRequestID = randomHex32()
		}
	}
	req.Header.Set("X-Conversation-Request-ID", conversationRequestID)
	req.Header.Set("X-Root-Request-ID", conversationRequestID)

	var convID string
	if clientReq != nil {
		convID = strings.TrimSpace(clientReq.Header.Get("X-Conversation-ID"))
	}
	if convID == "" && clientBody != nil {
		convID = strings.TrimSpace(stringValue(clientBody["conversationId"]))
	}
	if convID != "" {
		req.Header.Set("X-Conversation-ID", convID)
	}

	messageID := randomHex32()
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-B3-Traceid", messageID)
	req.Header.Set("X-B3-Spanid", randomHex32()[:16])
}

// injectDeepSeekThinking: deepseek* models need thinking.type=enabled + effort.
func injectDeepSeekThinking(payload map[string]any, model string) {
	if !isDeepSeekModelName(model) {
		return
	}
	effort := normalizeReasoningEffort(payload)
	// Explicit disabled → strip effort (official behavior).
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		if typ, _ := thinking["type"].(string); strings.EqualFold(typ, "disabled") {
			delete(payload, "reasoning_effort")
			delete(payload, "reasoningEffort")
			return
		}
		if typ, _ := thinking["type"].(string); strings.EqualFold(typ, "enabled") {
			if effort == "" {
				payload["reasoning_effort"] = "high"
			}
			return
		}
	}
	if effort == "" {
		payload["thinking"] = map[string]any{"type": "enabled"}
		payload["reasoning_effort"] = "high"
		return
	}
	// Already has effort but no thinking switch.
	if _, ok := payload["thinking"]; !ok {
		payload["thinking"] = map[string]any{"type": "enabled"}
	}
}

func isDeepSeekModelName(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

func normalizeReasoningEffort(payload map[string]any) string {
	if v, ok := payload["reasoning_effort"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if v, ok := payload["reasoningEffort"].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	if r, ok := payload["reasoning"].(map[string]any); ok {
		if v, ok := r["effort"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// backfillReasoningContent ensures all assistant messages carry
// reasoning_content once any has reasoning traces (DeepSeek multi-turn).
func backfillReasoningContent(payload map[string]any) {
	model, _ := payload["model"].(string)
	if !isDeepSeekModelName(model) {
		return
	}
	msgs, ok := payload["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !hasTrace {
		return
	}
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := msg["reasoning_content"]; ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			msg["reasoning_content"] = r
		} else {
			msg["reasoning_content"] = ""
		}
	}
}

const codebuddyNeutralSystemPrompt = "You are a helpful AI assistant."

// applySystemPromptMode:
//   - "custom": replace system/developer with gateway-owned prompt
//   - "passthrough" (default): keep client system; degraded retry injects neutral
func applySystemPromptMode(payload map[string]any, mode string, degraded bool) {
	if mode == "custom" {
		rewriteSystemMessages(payload, codebuddyNeutralSystemPrompt, true)
		return
	}
	if degraded {
		rewriteSystemMessages(payload, codebuddyNeutralSystemPrompt, true)
	}
}

func rewriteSystemMessages(payload map[string]any, systemText string, dropDeveloper bool) {
	msgs, ok := payload["messages"].([]any)
	if !ok {
		payload["messages"] = []any{
			map[string]any{"role": "system", "content": systemText},
		}
		return
	}
	out := make([]any, 0, len(msgs)+1)
	replaced := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system":
			if !replaced {
				out = append(out, map[string]any{"role": "system", "content": systemText})
				replaced = true
			}
		case "developer":
			if dropDeveloper {
				continue
			}
			out = append(out, mm)
		default:
			out = append(out, mm)
		}
	}
	if !replaced {
		out = append([]any{map[string]any{"role": "system", "content": systemText}}, out...)
	}
	payload["messages"] = out
}

// ── fingerprint sanitize (blacklist strips + one-word rewrites) ─────────────

var sanitizeFeatures = []string{
	"x-anthropic-billing-header",
	"cc_entrypoint=",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
	"11128",
}

var (
	sanitizeHdrRe     = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)
	sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)
	sanitizeKvRe      = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)
)

var sanitizeRewrites = [][2]string{
	{"You are Claude Code, Anthropic's official CLI for Claude", "You are Claude Code, Anthropic's official CLI tool for Claude"},
	{"Main branch (you will usually use this for PRs)", "Default branch (you will usually use this for PRs)"},
	{"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.", "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant."},
	{"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues", "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"},
	{"11128", "11-128"},
}

func hasFingerprintFeature(s string) bool {
	for _, feature := range sanitizeFeatures {
		if strings.Contains(s, feature) {
			return true
		}
	}
	return false
}

func sanitizeText(s string) string {
	if !hasFingerprintFeature(s) {
		return s
	}
	s = sanitizeHdrRe.ReplaceAllString(s, "")
	s = sanitizeBareHdrRe.ReplaceAllStringFunc(s, func(m string) string {
		return "X-Anthropic-Billing-Hdr"
	})
	s = sanitizeKvRe.ReplaceAllString(s, "")
	for _, pair := range sanitizeRewrites {
		s = strings.ReplaceAll(s, pair[0], pair[1])
	}
	return s
}

func sanitizeRequestPayload(payload map[string]any) {
	msgs, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			if hasFingerprintFeature(content) {
				msg["content"] = sanitizeText(content)
			}
		case []any:
			for _, part := range content {
				if m, ok := part.(map[string]any); ok {
					if text, ok := m["text"].(string); ok && hasFingerprintFeature(text) {
						m["text"] = sanitizeText(text)
					}
				}
			}
		}
	}
}

// isContentBlockedBody matches upstream 11128 fingerprint interception.
func isContentBlockedBody(body string) bool {
	return codebuddyLooksContentBlocked(400, body) || strings.Contains(body, "11128")
}

// unusedJson keeps encoding/json imported when tests trim usage.
var _ = json.Marshal
