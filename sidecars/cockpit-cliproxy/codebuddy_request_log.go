package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// codebuddy_request_log.go — per-request ledger for the CodeBuddy gateway.
//
// Stored as a ring buffer on disk so restarts keep recent history. The same
// path that applies 6004 / 429 governance also appends here, which keeps
// "which account hit which model limit" reconstructable without opening logs.

const (
	codebuddyRequestLogMax  = 500
	codebuddyRequestLogName = "codebuddy_request_log.json"
)

type codebuddyRequestRecord struct {
	ID                    string  `json:"id"`
	Timestamp             string  `json:"timestamp"`
	TimestampUnixMs       int64   `json:"timestampUnixMs"`
	AccountID             string  `json:"accountId,omitempty"`
	AccountLabel          string  `json:"accountLabel,omitempty"`
	APIKeyID              string  `json:"apiKeyId,omitempty"`
	APIKeyLabel           string  `json:"apiKeyLabel,omitempty"`
	Model                 string  `json:"model"`
	ClientStream          bool    `json:"clientStream"`
	Outcome               string  `json:"outcome"` // ok | rate_limited | quota | auth | error | cooling | content_blocked | invalid
	HTTPStatus            int     `json:"httpStatus,omitempty"`
	LatencyMs             int64   `json:"latencyMs,omitempty"`
	Message               string  `json:"message,omitempty"`
	ReasonCode            string  `json:"reasonCode,omitempty"` // 6004 | 11140 | soft_rate | breaker | ...
	ResetAt               string  `json:"resetAt,omitempty"`
	ConversationRequestID string  `json:"conversationRequestId,omitempty"`
	PromptTokens          int64   `json:"promptTokens,omitempty"`
	CompletionTokens      int64   `json:"completionTokens,omitempty"`
	TotalTokens           int64   `json:"totalTokens,omitempty"`
	Credit                float64 `json:"credit,omitempty"`
	HasCredit             bool    `json:"hasCredit,omitempty"`
	// Request metadata (no message content).
	MaxTokens        *int64   `json:"maxTokens,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"topP,omitempty"`
	MessageCount     int      `json:"messageCount,omitempty"`
	SystemChars      int      `json:"systemChars,omitempty"`
	ToolCount        int      `json:"toolCount,omitempty"`
	ToolChoice       string   `json:"toolChoice,omitempty"`
	FinishReason     string   `json:"finishReason,omitempty"`
	Attempt          int      `json:"attempt,omitempty"`
	DegradedPrompt   bool     `json:"degradedPrompt,omitempty"`
	PromptMode       string   `json:"promptMode,omitempty"`
	IncludeReasoning bool     `json:"includeReasoning,omitempty"`
	// Response extras (sanitized before client; captured from usage when present).
	CachedTokens     int64 `json:"cachedTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoningTokens,omitempty"`
	UpstreamStream   bool  `json:"upstreamStream,omitempty"`
	FirstTokenMs     int64 `json:"firstTokenMs,omitempty"`
	TotalMs          int64 `json:"totalMs,omitempty"`
}

type codebuddyRequestLogStore struct {
	mu       sync.Mutex
	path     string
	records  []codebuddyRequestRecord
	loaded   bool
	writeSeq uint64
}

var codebuddyReqLog *codebuddyRequestLogStore

func codebuddyRequestLogPath() string {
	// Reuse governance state directory.
	if dir := strings.TrimSpace(os.Getenv("COCKPIT_CODEBUDDY_STATE_DIR")); dir != "" {
		return filepath.Join(dir, codebuddyRequestLogName)
	}
	if dir := strings.TrimSpace(os.Getenv("COCKPIT_TOOLS_DATA_DIR")); dir != "" {
		return filepath.Join(dir, codebuddyRequestLogName)
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return codebuddyRequestLogName
	}
	return filepath.Join(home, ".antigravity_cockpit", codebuddyRequestLogName)
}

func codebuddyRequestLogInit() *codebuddyRequestLogStore {
	if codebuddyReqLog != nil {
		return codebuddyReqLog
	}
	store := &codebuddyRequestLogStore{path: codebuddyRequestLogPath()}
	store.load()
	codebuddyReqLog = store
	return store
}

func (s *codebuddyRequestLogStore) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var records []codebuddyRequestRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return
	}
	if len(records) > codebuddyRequestLogMax {
		records = records[:codebuddyRequestLogMax]
	}
	s.records = records
}

func (s *codebuddyRequestLogStore) persistLocked() {
	raw, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o755)
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// Append records one request attempt (including multi-account rotation steps).
func (s *codebuddyRequestLogStore) Append(rec codebuddyRequestRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if rec.ID == "" {
		s.writeSeq++
		rec.ID = time.Now().Format("20060102T150405") + "-" + itoa(s.writeSeq)
	}
	if rec.Timestamp == "" {
		now := time.Now()
		rec.Timestamp = now.Format(time.RFC3339)
		rec.TimestampUnixMs = now.UnixMilli()
	}
	s.records = append([]codebuddyRequestRecord{rec}, s.records...)
	if len(s.records) > codebuddyRequestLogMax {
		s.records = s.records[:codebuddyRequestLogMax]
	}
	s.persistLocked()
}

func (s *codebuddyRequestLogStore) List(limit int) []codebuddyRequestRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if limit <= 0 || limit > len(s.records) {
		limit = len(s.records)
	}
	out := make([]codebuddyRequestRecord, limit)
	copy(out, s.records[:limit])
	return out
}

// ListPage returns a window of records (newest first) for UI pagination.
// apiKey empty = all; otherwise filter by apiKeyId / apiKeyLabel contains.
func (s *codebuddyRequestLogStore) ListPage(offset, limit int, apiKey string) (records []codebuddyRequestRecord, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	apiKey = strings.TrimSpace(apiKey)
	filtered := s.records
	if apiKey != "" {
		filtered = nil
		for _, rec := range s.records {
			if strings.Contains(rec.APIKeyID, apiKey) || strings.Contains(rec.APIKeyLabel, apiKey) ||
				rec.APIKeyID == apiKey || rec.APIKeyLabel == apiKey {
				filtered = append(filtered, rec)
			}
		}
	}
	total = len(filtered)
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		return []codebuddyRequestRecord{}, total
	}
	if limit <= 0 {
		limit = 20
	}
	end := offset + limit
	if end > total {
		end = total
	}
	out := make([]codebuddyRequestRecord, end-offset)
	// Newest first within filtered slice (records are append-ordered oldest→newest).
	for i := 0; i < end-offset; i++ {
		out[i] = filtered[total-1-(offset+i)]
	}
	return out, total
}

// StatsByAPIKey aggregates ok/fail/tokens/credit per client API key.
func (s *codebuddyRequestLogStore) StatsByAPIKey() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	type agg struct {
		id, label string
		ok, fail  int
		credit    float64
		tokens    int64
		lastAt    string
	}
	m := map[string]*agg{}
	for _, rec := range s.records {
		key := rec.APIKeyID
		if key == "" {
			key = rec.APIKeyLabel
		}
		if key == "" {
			key = "(unknown)"
		}
		a := m[key]
		if a == nil {
			a = &agg{id: rec.APIKeyID, label: rec.APIKeyLabel}
			m[key] = a
		}
		if a.label == "" {
			a.label = rec.APIKeyLabel
		}
		if rec.Outcome == "ok" {
			a.ok++
			a.credit += rec.Credit
		} else {
			a.fail++
		}
		a.tokens += rec.TotalTokens
		if rec.Timestamp > a.lastAt {
			a.lastAt = rec.Timestamp
		}
	}
	out := map[string]any{}
	for k, v := range m {
		out[k] = map[string]any{
			"apiKeyId":    v.id,
			"apiKeyLabel": v.label,
			"ok":          v.ok,
			"fail":        v.fail,
			"credit":      v.credit,
			"tokens":      v.tokens,
			"requests":    v.ok + v.fail,
			"lastAt":      v.lastAt,
		}
	}
	return out
}

func (s *codebuddyRequestLogStore) Total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return len(s.records)
}

func (s *codebuddyRequestLogStore) StatsByAccountModel() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	type key struct{ account, model string }
	counts := map[key]*struct {
		ok, fail int
		credit   float64
	}{}
	for i := len(s.records) - 1; i >= 0; i-- {
		rec := s.records[i]
		if rec.AccountID == "" || rec.Model == "" {
			continue
		}
		k := key{rec.AccountID, rec.Model}
		if counts[k] == nil {
			counts[k] = &struct {
				ok, fail int
				credit   float64
			}{}
		}
		if rec.Outcome == "ok" {
			counts[k].ok++
			counts[k].credit += rec.Credit
		} else {
			counts[k].fail++
		}
	}
	out := map[string]any{}
	for k, v := range counts {
		id := k.account + "|" + k.model
		out[id] = map[string]any{
			"accountId": k.account,
			"model":     k.model,
			"ok":        v.ok,
			"fail":      v.fail,
			"credit":    v.credit,
		}
	}
	return out
}

// normalizeUsageAccounting maps upstream usage fields onto Codex-style columns.
//
// DeepSeek/CodeBuddy typically report prompt_tokens as the FULL input including
// cache hits; cache_read is a subset of prompt. Doubling (prompt+cacheRead)
// understates hit rate (~50% when true hit is ~99%).
func normalizeUsageAccounting(prompt, cacheRead, cacheWrite, completion int64) (inputNew, cacheR, cacheW, totalInput, output int64) {
	cacheR = cacheRead
	cacheW = cacheWrite
	output = completion
	if cacheRead > 0 && cacheRead <= prompt {
		// prompt already includes cache hits
		totalInput = prompt
		if prompt-cacheRead > 0 {
			inputNew = prompt - cacheRead
		} else {
			inputNew = cacheWrite
		}
		if cacheW == 0 {
			cacheW = inputNew
		}
		return
	}
	// prompt is uncached-only (or no cache reported)
	inputNew = prompt
	totalInput = prompt + cacheRead
	return
}

// ModelUsageStats aggregates token/cache usage per model (Codex-style table).
func (s *codebuddyRequestLogStore) ModelUsageStats() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	type agg struct {
		model                                              string
		requests                                           int64
		inputNew, completion, cacheRead, cacheWrite, total int64
		credit                                             float64
		lastAt                                             string
	}
	m := map[string]*agg{}
	for _, rec := range s.records {
		if rec.Outcome != "ok" || rec.Model == "" {
			continue
		}
		a := m[rec.Model]
		if a == nil {
			a = &agg{model: rec.Model}
			m[rec.Model] = a
		}
		inNew, cR, cW, totalIn, out := normalizeUsageAccounting(
			rec.PromptTokens, rec.CachedTokens, rec.CacheWriteTokens, rec.CompletionTokens,
		)
		a.requests++
		a.inputNew += inNew
		a.completion += out
		a.cacheRead += cR
		a.cacheWrite += cW
		if rec.TotalTokens > 0 {
			a.total += rec.TotalTokens
		} else {
			a.total += totalIn + out
		}
		a.credit += rec.Credit
		if rec.Timestamp > a.lastAt {
			a.lastAt = rec.Timestamp
		}
	}
	out := make([]map[string]any, 0, len(m))
	for _, a := range m {
		totalInput := a.inputNew + a.cacheRead
		hit := 0.0
		if totalInput > 0 {
			hit = float64(a.cacheRead) / float64(totalInput) * 100
		}
		out = append(out, map[string]any{
			"model":        a.model,
			"requests":     a.requests,
			"inputTokens":  a.inputNew,
			"cacheRead":    a.cacheRead,
			"cacheWrite":   a.cacheWrite,
			"totalInput":   totalInput,
			"outputTokens": a.completion,
			"cacheHitPct":  hit,
			"totalTokens":  a.total,
			"credit":       a.credit,
			"lastAt":       a.lastAt,
		})
	}
	return out
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// extractRequestMeta snapshots non-content request parameters for the ledger.
func extractRequestMeta(payload map[string]any) (maxTokens *int64, temperature, topP *float64, msgCount, systemChars, toolCount int, toolChoice string) {
	if payload == nil {
		return nil, nil, nil, 0, 0, 0, ""
	}
	if v, ok := payload["max_tokens"].(float64); ok {
		n := int64(v)
		maxTokens = &n
	}
	if v, ok := payload["temperature"].(float64); ok {
		f := v
		temperature = &f
	}
	if v, ok := payload["top_p"].(float64); ok {
		f := v
		topP = &f
	}
	if msgs, ok := payload["messages"].([]any); ok {
		msgCount = len(msgs)
		for _, mm := range msgs {
			msg, ok := mm.(map[string]any)
			if !ok {
				continue
			}
			role, _ := msg["role"].(string)
			if role != "system" && role != "developer" {
				continue
			}
			switch content := msg["content"].(type) {
			case string:
				systemChars += len(content)
			case []any:
				for _, part := range content {
					if m, ok := part.(map[string]any); ok {
						if text, ok := m["text"].(string); ok {
							systemChars += len(text)
						}
					}
				}
			}
		}
	}
	if tools, ok := payload["tools"].([]any); ok {
		toolCount = len(tools)
	}
	switch v := payload["tool_choice"].(type) {
	case string:
		toolChoice = v
	case map[string]any:
		if typ, ok := v["type"].(string); ok {
			toolChoice = typ
		}
	}
	return maxTokens, temperature, topP, msgCount, systemChars, toolCount, toolChoice
}

func codebuddyRecordRequest(rec codebuddyRequestRecord) {
	codebuddyRequestLogInit().Append(rec)
	emitCodebuddyUsageEvent(rec)
}

func emitCodebuddyUsageEvent(rec codebuddyRequestRecord) {
	emitter := globalCodebuddyEmitter
	if emitter == nil {
		return
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		obj = map[string]any{}
	}
	obj["type"] = "codebuddy_usage"
	emitter.emit(obj)
}

// classifyCodebuddyOutcome maps HTTP/body into a stable outcome + reason code.
func classifyCodebuddyOutcome(status int, rawBody string) (outcome, reasonCode string) {
	switch {
	case status >= 200 && status < 300:
		return "ok", ""
	case isModelRateLimit(rawBody):
		return "rate_limited", codebuddyModelRateCode
	case status == http.StatusTooManyRequests:
		return "rate_limited", "soft_rate"
	case codebuddyIsHardQuota(rawBody, status):
		return "quota", "hard_quota"
	case status == http.StatusUnauthorized:
		return "auth", "auth_failed"
	case codebuddyLooksContentBlocked(status, rawBody):
		return "content_blocked", "11128"
	case status == 0:
		return "error", "network"
	default:
		return "error", "upstream"
	}
}
