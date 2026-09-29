package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// codebuddy_governance.go ports workbuddy2api traffic governance into the
// CodeBuddy/WorkBuddy local API gateway:
//
//   - 429 soft cooldown (600s base, exponential, cap soft_rate_max=2h)
//   - 429 code 6004 → model-level cooldown only (switch model stays usable)
//   - 404 shallow cooldown (30s)
//   - 402 / balance exhausted → hard cooldown until next 04:00 CST
//   - consecutive failure circuit breaker (threshold 5, exp backoff cap 6h)
//   - atomic state.json persistence, restore on boot
//
// Redis mirror is intentionally omitted here (local-first); Cockpit owns the
// process lifecycle and can snapshot if needed.

const (
	codebuddySoftCooldownBase = 600 * time.Second
	codebuddySoftCooldownMax  = 2 * time.Hour
	codebuddyShallowCooldown  = 30 * time.Second
	codebuddyBreakerThreshold = 5
	codebuddyBreakerBase      = 60 * time.Second
	codebuddyBreakerMax       = 6 * time.Hour
	codebuddyModelRateCode    = "6004"
)

type codebuddyCoolKind string

const (
	codebuddyCoolNone       codebuddyCoolKind = ""
	codebuddyCoolSoftRate   codebuddyCoolKind = "soft_rate"
	codebuddyCoolSoftModel  codebuddyCoolKind = "soft_model"
	codebuddyCoolShallow    codebuddyCoolKind = "shallow"
	codebuddyCoolHardQuota  codebuddyCoolKind = "hard_quota"
	codebuddyCoolBreaker    codebuddyCoolKind = "breaker"
	codebuddyCoolAuthFailed codebuddyCoolKind = "auth_failed"
)

type codebuddyModelCooldown struct {
	Until   time.Time `json:"until"`
	ResetAt time.Time `json:"resetAt,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

type codebuddyAccountGovernance struct {
	Until            time.Time                          `json:"until,omitempty"`
	CoolKind         codebuddyCoolKind                  `json:"coolKind,omitempty"`
	Reason           string                             `json:"reason,omitempty"`
	SoftStreak       int                                `json:"softStreak,omitempty"`
	BreakerFailures  int                                `json:"breakerFailures,omitempty"`
	ModelCooldowns   map[string]codebuddyModelCooldown `json:"modelCooldowns,omitempty"`
	UpdatedAt        time.Time                          `json:"updatedAt"`
}

type codebuddyGovernanceState struct {
	Version  int                                  `json:"version"`
	Accounts map[string]*codebuddyAccountGovernance `json:"accounts"`
}

type codebuddyGovernance struct {
	mu       sync.Mutex
	path     string
	accounts map[string]*codebuddyAccountGovernance
	dirty    bool
}

var codebuddyGov *codebuddyGovernance

func codebuddyGovernancePath() string {
	if dir := strings.TrimSpace(os.Getenv("COCKPIT_CODEBUDDY_STATE_DIR")); dir != "" {
		return filepath.Join(dir, "codebuddy_pool_state.json")
	}
	if dir := strings.TrimSpace(os.Getenv("COCKPIT_TOOLS_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "codebuddy_pool_state.json")
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return "codebuddy_pool_state.json"
	}
	return filepath.Join(home, ".antigravity_cockpit", "codebuddy_pool_state.json")
}

func codebuddyGovernanceInit() *codebuddyGovernance {
	if codebuddyGov != nil {
		return codebuddyGov
	}
	g := &codebuddyGovernance{
		path:     codebuddyGovernancePath(),
		accounts: map[string]*codebuddyAccountGovernance{},
	}
	g.load()
	codebuddyGov = g
	return g
}

func (g *codebuddyGovernance) load() {
	raw, err := os.ReadFile(g.path)
	if err != nil {
		return
	}
	var state codebuddyGovernanceState
	if err := json.Unmarshal(raw, &state); err != nil {
		return
	}
	if state.Accounts != nil {
		g.accounts = state.Accounts
	}
}

func (g *codebuddyGovernance) persistLocked() {
	if g.path == "" {
		return
	}
	state := codebuddyGovernanceState{Version: 1, Accounts: g.accounts}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(g.path), 0o755)
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, g.path)
	g.dirty = false
}

func (g *codebuddyGovernance) entryLocked(id string) *codebuddyAccountGovernance {
	if g.accounts == nil {
		g.accounts = map[string]*codebuddyAccountGovernance{}
	}
	e := g.accounts[id]
	if e == nil {
		e = &codebuddyAccountGovernance{}
		g.accounts[id] = e
	}
	return e
}

func (g *codebuddyGovernance) available(id, model string) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.accounts[id]
	if e == nil {
		return true, ""
	}
	now := time.Now()
	if !e.Until.IsZero() && now.Before(e.Until) {
		return false, fmt.Sprintf("account %s cooling (%s until %s)", id, e.CoolKind, e.Until.Format(time.RFC3339))
	}
	if model != "" && e.ModelCooldowns != nil {
		if mc, ok := e.ModelCooldowns[model]; ok && now.Before(mc.Until) {
			return false, fmt.Sprintf("model %s rate-limited on account %s until %s", model, id, mc.Until.Format(time.RFC3339))
		}
	}
	return true, ""
}

func (g *codebuddyGovernance) rateLimitedModels(id string) []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.accounts[id]
	if e == nil || len(e.ModelCooldowns) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(e.ModelCooldowns))
	for model, mc := range e.ModelCooldowns {
		if now.Before(mc.Until) {
			item := map[string]any{
				"model": model,
				"until": mc.Until.UTC().Format(time.RFC3339),
			}
			if !mc.ResetAt.IsZero() {
				item["resetAt"] = mc.ResetAt.UTC().Format(time.RFC3339)
			}
			if mc.Reason != "" {
				item["reason"] = mc.Reason
			}
			out = append(out, item)
		}
	}
	return out
}

func (g *codebuddyGovernance) noteSuccess(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.accounts[id]
	if e == nil {
		return
	}
	e.SoftStreak = 0
	e.BreakerFailures = 0
	e.UpdatedAt = time.Now()
	g.dirty = true
	g.persistLocked()
}

func (g *codebuddyGovernance) coolSoftRate(id string, resetAt time.Time, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entryLocked(id)
	now := time.Now()
	if !resetAt.IsZero() {
		until := resetAt
		if until.After(now.Add(codebuddySoftCooldownMax)) {
			until = now.Add(codebuddySoftCooldownMax)
		}
		e.Until = until
		e.CoolKind = codebuddyCoolSoftRate
		e.Reason = reason
		e.ModelCooldowns = nil
	} else {
		// Do not stack while already cooling.
		if e.CoolKind != codebuddyCoolSoftRate || !now.Before(e.Until) {
			e.SoftStreak++
			d := codebuddySoftCooldownBase << (e.SoftStreak - 1)
			if e.SoftStreak > 4 || d > codebuddySoftCooldownMax || d <= 0 {
				d = codebuddySoftCooldownMax
			}
			e.Until = now.Add(d)
		}
		e.CoolKind = codebuddyCoolSoftRate
		e.Reason = reason
		e.ModelCooldowns = nil
	}
	e.UpdatedAt = now
	g.dirty = true
	g.persistLocked()
}

func (g *codebuddyGovernance) coolSoftModel(id, model string, resetAt time.Time, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entryLocked(id)
	now := time.Now()
	if e.ModelCooldowns == nil {
		e.ModelCooldowns = map[string]codebuddyModelCooldown{}
	}
	until := resetAt
	if until.IsZero() {
		// No upstream reset wall-clock: cool **this model only** for a short
		// window instead of locking the whole account (do not hammer upstream).
		until = now.Add(5 * time.Minute)
		e.ModelCooldowns[model] = codebuddyModelCooldown{Until: until, Reason: reason}
		e.UpdatedAt = now
		g.dirty = true
		g.persistLocked()
		return
	}
	if until.After(now.Add(codebuddySoftCooldownMax)) {
		until = now.Add(codebuddySoftCooldownMax)
	}
	e.ModelCooldowns[model] = codebuddyModelCooldown{
		Until:   until,
		ResetAt: resetAt,
		Reason:  reason,
	}
	// Model-level only: do not write account-level until.
	e.UpdatedAt = now
	g.dirty = true
	g.persistLocked()
}

func (g *codebuddyGovernance) coolShallow(id, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entryLocked(id)
	e.Until = time.Now().Add(codebuddyShallowCooldown)
	e.CoolKind = codebuddyCoolShallow
	e.Reason = reason
	e.ModelCooldowns = nil
	e.UpdatedAt = time.Now()
	g.dirty = true
	g.persistLocked()
}

func (g *codebuddyGovernance) coolHardQuota(id, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entryLocked(id)
	e.Until = codebuddyNext0400CST()
	e.CoolKind = codebuddyCoolHardQuota
	e.Reason = reason
	e.ModelCooldowns = nil
	e.UpdatedAt = time.Now()
	g.dirty = true
	g.persistLocked()
}

func (g *codebuddyGovernance) noteFailure(id string, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entryLocked(id)
	e.BreakerFailures++
	e.UpdatedAt = time.Now()
	if e.BreakerFailures >= codebuddyBreakerThreshold {
		shift := e.BreakerFailures - codebuddyBreakerThreshold
		if shift > 6 {
			shift = 6
		}
		d := codebuddyBreakerBase << shift
		if d > codebuddyBreakerMax || d <= 0 {
			d = codebuddyBreakerMax
		}
		e.Until = time.Now().Add(d)
		e.CoolKind = codebuddyCoolBreaker
		e.Reason = reason
		e.ModelCooldowns = nil
	}
	g.dirty = true
	g.persistLocked()
}

func (g *codebuddyGovernance) markAuthFailed(id, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.entryLocked(id)
	e.Until = time.Now().Add(30 * time.Minute)
	e.CoolKind = codebuddyCoolAuthFailed
	e.Reason = reason
	e.UpdatedAt = time.Now()
	g.dirty = true
	g.persistLocked()
}

// statusLedger returns per-account cooling info for diagnostics.
func (g *codebuddyGovernance) statusLedger() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	out := make([]map[string]any, 0, len(g.accounts))
	for id, e := range g.accounts {
		item := map[string]any{"id": id}
		if !e.Until.IsZero() && now.Before(e.Until) {
			item["until"] = e.Until.UTC().Format(time.RFC3339)
			item["coolKind"] = string(e.CoolKind)
			if e.Reason != "" {
				item["reason"] = e.Reason
			}
		}
		if models := func() []map[string]any {
			if len(e.ModelCooldowns) == 0 {
				return nil
			}
			list := make([]map[string]any, 0, len(e.ModelCooldowns))
			for model, mc := range e.ModelCooldowns {
				if now.Before(mc.Until) {
					row := map[string]any{
						"model": model,
						"until": mc.Until.UTC().Format(time.RFC3339),
					}
					if !mc.ResetAt.IsZero() {
						row["resetAt"] = mc.ResetAt.UTC().Format(time.RFC3339)
					}
					list = append(list, row)
				}
			}
			return list
		}(); len(models) > 0 {
			item["rateLimitedModels"] = models
		}
		if e.BreakerFailures > 0 {
			item["breakerFailures"] = e.BreakerFailures
		}
		if len(item) > 1 {
			out = append(out, item)
		}
	}
	return out
}

func codebuddyNext0400CST() time.Time {
	// CST = UTC+8, no DST.
	now := time.Now().UTC()
	// 04:00 CST = 20:00 UTC previous calendar day boundary.
	// Compute next local 04:00 in +8.
	loc := time.FixedZone("CST", 8*3600)
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), 4, 0, 0, 0, loc)
	if !local.Before(next) {
		next = next.Add(24 * time.Hour)
	}
	return next.UTC()
}

// isModelRateLimit reports whether body is code 6004 model rate limit.
func isModelRateLimit(body string) bool {
	var decoded struct {
		Code json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return strings.Contains(body, `"code":6004`) || strings.Contains(body, `"code": 6004`) ||
			strings.Contains(body, `"code":"6004"`)
	}
	raw := strings.TrimSpace(string(decoded.Code))
	if raw == "" {
		return false
	}
	raw = strings.Trim(raw, `"`)
	return raw == codebuddyModelRateCode
}

// parseRateResetAt extracts rate-limit reset wall-clock.
// CN: "将在 YYYY-MM-DD HH:MM:SS UTC+8 重置"
// EN: "will reset at YYYY-MM-DD HH:MM:SS" (global domain; workbuddy2api f044e5c)
func parseRateResetAt(body string) time.Time {
	loc := time.FixedZone("UTC+8", 8*3600)
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
	}
	parse := func(raw string) time.Time {
		raw = strings.TrimSpace(raw)
		raw = strings.TrimSuffix(raw, "UTC+8")
		raw = strings.TrimSpace(raw)
		raw = strings.TrimSuffix(raw, "UTC +8")
		raw = strings.TrimSpace(raw)
		for _, layout := range layouts {
			if ts, err := time.ParseInLocation(layout, raw, loc); err == nil {
				return ts.UTC()
			}
		}
		return time.Time{}
	}
	// Chinese form.
	if idx := strings.Index(body, "将在"); idx >= 0 {
		rest := body[idx+len("将在"):]
		if end := strings.Index(rest, "重置"); end >= 0 {
			rest = rest[:end]
		}
		if ts := parse(rest); !ts.IsZero() {
			return ts
		}
	}
	// English form: reset at <timestamp> (avoid matching "reset at the end...").
	lower := strings.ToLower(body)
	marker := "reset at "
	idx := strings.Index(lower, marker)
	for idx >= 0 {
		rest := body[idx+len(marker):]
		// Only accept when next chars look like a date.
		trimmed := strings.TrimSpace(rest)
		if len(trimmed) >= 19 && trimmed[4] == '-' && trimmed[7] == '-' {
			// take up to first non-timestamp-ish delimiter
			end := 0
			for end < len(trimmed) {
				ch := trimmed[end]
				if (ch >= '0' && ch <= '9') || ch == '-' || ch == ':' || ch == 'T' || ch == '.' {
					end++
					continue
				}
				if ch == ' ' && end < 19 {
					end++
					continue
				}
				break
			}
			if ts := parse(trimmed[:end]); !ts.IsZero() {
				return ts
			}
		}
		next := strings.Index(lower[idx+len(marker):], marker)
		if next < 0 {
			break
		}
		idx = idx + len(marker) + next
	}
	return time.Time{}
}

func codebuddyIsHardQuota(body string, status int) bool {
	if status == httpStatusPaymentRequired {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "余额不足") ||
		strings.Contains(lower, "insufficient") ||
		strings.Contains(lower, "余额耗尽") ||
		strings.Contains(body, "11140") && strings.Contains(lower, "credit")
}

const httpStatusPaymentRequired = 402

func codebuddyLooksContentBlocked(status int, body string) bool {
	if status != 400 {
		return false
	}
	return strings.Contains(body, "11128") || strings.Contains(body, "内容审核") ||
		strings.Contains(strings.ToLower(body), "content") && strings.Contains(strings.ToLower(body), "block")
}

func parseOptionalInt(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// isClientAborted reports whether the error is a client disconnect / ctx cancel.
// These must NOT cool accounts or rotate the pool — the client left; the account is fine.
func isClientAborted(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "context canceled") ||
		strings.Contains(s, "context cancelled") ||
		strings.Contains(s, "client closed request") ||
		strings.Contains(s, "client disconnected")
}

// codebuddyKeyGovernance cools individual client API keys so one hammering
// client cannot burn through the whole account pool via repeated aborts.
type codebuddyKeyGovernance struct {
	mu     sync.Mutex
	until  map[string]time.Time
	streak map[string]int
}

var codebuddyKeys = &codebuddyKeyGovernance{
	until:  map[string]time.Time{},
	streak: map[string]int{},
}

func (k *codebuddyKeyGovernance) blocked(keyID string) (bool, string) {
	if keyID == "" {
		return false, ""
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	until, ok := k.until[keyID]
	if !ok || !time.Now().Before(until) {
		return false, ""
	}
	return true, fmt.Sprintf("API key cooling until %s", until.Format(time.RFC3339))
}

func (k *codebuddyKeyGovernance) cool(keyID string, d time.Duration, reason string) {
	if keyID == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.streak[keyID]++
	// Escalate on repeated aborts: 30s → 2m → 10m cap.
	shift := k.streak[keyID] - 1
	if shift > 4 {
		shift = 4
	}
	if d <= 0 {
		d = 30 * time.Second
	}
	eff := d << shift
	if eff > 10*time.Minute {
		eff = 10 * time.Minute
	}
	k.until[keyID] = time.Now().Add(eff)
	_ = reason
}

func (k *codebuddyKeyGovernance) noteOK(keyID string) {
	if keyID == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.streak[keyID] = 0
	delete(k.until, keyID)
}

func (k *codebuddyKeyGovernance) status() map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	out := map[string]string{}
	for id, until := range k.until {
		if now.Before(until) {
			out[id] = until.UTC().Format(time.RFC3339)
		}
	}
	return out
}
