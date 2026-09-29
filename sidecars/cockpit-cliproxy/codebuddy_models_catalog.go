package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// codebuddy_models_catalog.go — enterprise catalog + models.dev context +
// runtime cost observations + disable flags, exposed for UI and /v1/models.

const codebuddyModelsDevURL = "https://models.dev/api.json"
const codebuddyModelsDevTimeout = 5 * time.Second
const codebuddyModelsDevCooldown = 5 * time.Minute
const codebuddyDefaultContextWindow = int64(128000)

// Seed context/output limits (aligned with workbuddy2api internal/upstream/model.json).
var codebuddyModelSeed = map[string]map[string]int64{
	"glm-5.2":               {"context_length": 1000000, "max_output_tokens": 131072},
	"glm-5.1":               {"context_length": 200000, "max_output_tokens": 131072},
	"glm-5.3":               {"context_length": 1000000, "max_output_tokens": 131072},
	"glm-5.3-flash":         {"context_length": 1000000, "max_output_tokens": 131072},
	"glm-5v-turbo":          {"context_length": 200000, "max_output_tokens": 131072},
	"kimi-k2.7":             {"context_length": 256000, "max_output_tokens": 65536},
	"kimi-k2.6":             {"context_length": 256000, "max_output_tokens": 262144},
	"kimi-k2.5":             {"context_length": 164000, "max_output_tokens": 262144},
	"kimi-k3":               {"context_length": 1048576, "max_output_tokens": 131072},
	"kimi-k2.8-preview":     {"context_length": 1048576, "max_output_tokens": 131072},
	"minimax-m3":            {"context_length": 512000, "max_output_tokens": 512000},
	"hy3":                   {"context_length": 192000, "max_output_tokens": 64000},
	"hy3-preview":           {"context_length": 262144, "max_output_tokens": 64000},
	"hy4-preview":           {"context_length": 1000000, "max_output_tokens": 64000},
	"hy4-preview-x":         {"context_length": 1000000, "max_output_tokens": 64000},
	"deepseek-v4-pro":       {"context_length": 1000000, "max_output_tokens": 384000},
	"deepseek-v4-flash":     {"context_length": 1000000, "max_output_tokens": 384000},
	"deepseek-v4.1-flash":   {"context_length": 1000000, "max_output_tokens": 384000},
	"gpt-5.5":               {"context_length": 1050000, "max_output_tokens": 128000},
	"gpt-5.4":               {"context_length": 1050000, "max_output_tokens": 128000},
	"gpt-5.3-codex":         {"context_length": 400000, "max_output_tokens": 128000},
	"gemini-3.5-flash":      {"context_length": 1048576, "max_output_tokens": 65536},
	"auto":                  {"context_length": 168000, "max_output_tokens": 64000},
}

var codebuddyModelsDevVendor = map[string]bool{
	"zai": true, "moonshotai": true, "moonshotai-cn": true,
	"openai": true, "google": true, "deepseek": true, "minimax": true,
}

type codebuddyModelsDevFetcher struct {
	mu         sync.Mutex
	index      map[string]map[string]int64
	negatives  map[string]time.Time
	lastFetch  time.Time
	fetching   bool
	cachePath  string
}

var (
	codebuddyModelsDevOnce sync.Once
	codebuddyModelsDevInst *codebuddyModelsDevFetcher
)

func codebuddyCatalogCachePath() string {
	if dir := strings.TrimSpace(os.Getenv("COCKPIT_CODEBUDDY_STATE_DIR")); dir != "" {
		return filepath.Join(dir, "codebuddy_model_catalog.json")
	}
	if dir := strings.TrimSpace(os.Getenv("COCKPIT_TOOLS_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "codebuddy_model_catalog.json")
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return "codebuddy_model_catalog.json"
	}
	return filepath.Join(home, ".antigravity_cockpit", "codebuddy_model_catalog.json")
}

func codebuddyModelsDev() *codebuddyModelsDevFetcher {
	codebuddyModelsDevOnce.Do(func() {
		f := &codebuddyModelsDevFetcher{
			index:     map[string]map[string]int64{},
			negatives: map[string]time.Time{},
			cachePath: codebuddyCatalogCachePath(),
		}
		f.loadCache()
		codebuddyModelsDevInst = f
	})
	return codebuddyModelsDevInst
}

func (f *codebuddyModelsDevFetcher) loadCache() {
	raw, err := os.ReadFile(f.cachePath)
	if err != nil {
		return
	}
	var payload struct {
		Models map[string]map[string]int64 `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	if payload.Models != nil {
		f.index = payload.Models
	}
}

func (f *codebuddyModelsDevFetcher) saveCacheLocked() {
	if f.cachePath == "" {
		return
	}
	payload := map[string]any{"models": f.index, "savedAt": time.Now().UTC().Format(time.RFC3339)}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(f.cachePath), 0o755)
	tmp := f.cachePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, f.cachePath)
}

func (f *codebuddyModelsDevFetcher) lookup(model string) (ctx, out int64, ok bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	f.mu.Lock()
	if e, hit := f.index[model]; hit {
		ctx, out = e["context_length"], e["max_output_tokens"]
		f.mu.Unlock()
		return ctx, out, true
	}
	if t, neg := f.negatives[model]; neg && time.Since(t) < 24*time.Hour {
		f.mu.Unlock()
		return 0, 0, false
	}
	needFetch := time.Since(f.lastFetch) > codebuddyModelsDevCooldown && !f.fetching
	if needFetch {
		f.fetching = true
		f.lastFetch = time.Now()
		go f.fetchDoc()
	}
	f.mu.Unlock()
	return 0, 0, false
}

func (f *codebuddyModelsDevFetcher) fetchDoc() {
	defer func() {
		f.mu.Lock()
		f.fetching = false
		f.mu.Unlock()
	}()
	client := &http.Client{Timeout: codebuddyModelsDevTimeout}
	resp, err := client.Get(codebuddyModelsDevURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return
	}
	type cand struct {
		ctx, out int64
		provider string
		vendor   bool
		votes    int
	}
	buckets := map[string]map[string]*cand{}
	for provider, node := range generic {
		pobj, ok := node.(map[string]any)
		if !ok {
			continue
		}
		models, _ := pobj["models"].(map[string]any)
		for mid, mnode := range models {
			mobj, ok := mnode.(map[string]any)
			if !ok {
				continue
			}
			limit, _ := mobj["limit"].(map[string]any)
			if limit == nil {
				continue
			}
			ctx, _ := toFloat64(limit["context"])
			out, _ := toFloat64(limit["output"])
			if ctx <= 0 || ctx > 1e9 {
				continue
			}
			bare := mid
			if i := strings.LastIndex(mid, "/"); i >= 0 {
				bare = mid[i+1:]
			}
			bare = strings.ToLower(bare)
			key := strings.Join([]string{fmtF(ctx), fmtF(out)}, "|")
			if buckets[bare] == nil {
				buckets[bare] = map[string]*cand{}
			}
			c := buckets[bare][key]
			if c == nil {
				c = &cand{ctx: int64(ctx), out: int64(out), provider: provider, vendor: codebuddyModelsDevVendor[strings.ToLower(provider)]}
				buckets[bare][key] = c
			}
			c.votes++
			if codebuddyModelsDevVendor[strings.ToLower(provider)] {
				c.vendor = true
				c.provider = provider
			}
		}
	}
	f.mu.Lock()
	for bare, byVal := range buckets {
		var best *cand
		for _, c := range byVal {
			if best == nil {
				best = c
				continue
			}
			if c.vendor && !best.vendor {
				best = c
				continue
			}
			if c.vendor == best.vendor {
				if c.votes > best.votes || (c.votes == best.votes && c.provider < best.provider) {
					best = c
				}
			}
		}
		if best != nil {
			f.index[bare] = map[string]int64{"context_length": best.ctx, "max_output_tokens": best.out}
		}
	}
	f.saveCacheLocked()
	f.mu.Unlock()
}

func toFloat64(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

func fmtF(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// codebuddyModelCatalogRow is the UI/status shape for one model.
type codebuddyModelCatalogRow struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	ContextLength   int64    `json:"contextLength"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	Credits         string   `json:"credits,omitempty"`
	CreditsRate     float64  `json:"creditsRate"`
	Tags            []string `json:"tags,omitempty"`
	Vendor          string   `json:"vendor,omitempty"`
	IsDefault       bool     `json:"isDefault,omitempty"`
	OnlyReasoning   bool     `json:"onlyReasoning,omitempty"`
	MaxAllowedSize  int64    `json:"maxAllowedSize,omitempty"`
	Efforts         []string `json:"efforts,omitempty"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsReason  bool     `json:"supportsReasoning"`
	SupportsTools   bool     `json:"supportsToolCall"`
	CLI             bool     `json:"cli"`
	Disabled        bool     `json:"disabled"`
	// Runtime cost from pool ledger.
	FreeObserved  *bool    `json:"freeObserved,omitempty"`
	CostPer1k     *float64 `json:"costPer1k,omitempty"`
	CostSamples   int      `json:"costSamples,omitempty"`
	CostLastSeen  string   `json:"costLastSeen,omitempty"`
	CostTier      *int     `json:"costTier,omitempty"`
	// Per-account entitlement display (hy4-preview 全天免费 vs 仅夜间免费).
	AccountsAlwaysFree []string `json:"accountsAlwaysFree,omitempty"`
	AccountsNightFree  []string `json:"accountsNightFree,omitempty"`
	FreePolicySummary  string   `json:"freePolicySummary,omitempty"`
	// Request usage aggregate for this model.
	Requests     int64   `json:"requests"`
	TotalTokens  int64   `json:"totalTokens"`
	TotalCredit  float64 `json:"totalCredit"`
	Source       string  `json:"source,omitempty"`
	Description  string  `json:"description,omitempty"`
}

func summarizeAccountFree(always, night []string, now time.Time) string {
	var parts []string
	if len(always) > 0 {
		parts = append(parts, "全天免费:"+strings.Join(always, ","))
	}
	if len(night) > 0 {
		state := "窗外计费"
		if nightPolicyInWindow(now) {
			state = "夜间窗口内免费"
		}
		parts = append(parts, "仅夜间免费:"+strings.Join(night, ",")+"("+state+")")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

func parseCreditsRate(s string) float64 {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "x")
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	var f float64
	_, _ = fmtSscan(s, &f)
	return f
}

func fmtSscan(s string, f *float64) (int, error) {
	// tiny scanner without importing fmt for one call in hot path tests
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	var val float64
	var frac float64
	var div float64 = 1
	seenDot := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '.' {
			if seenDot {
				break
			}
			seenDot = true
			continue
		}
		if ch < '0' || ch > '9' {
			break
		}
		if !seenDot {
			val = val*10 + float64(ch-'0')
		} else {
			div *= 10
			frac = frac*10 + float64(ch-'0')
		}
	}
	val += frac / div
	if neg {
		val = -val
	}
	*f = val
	return 1, nil
}

// BuildCatalog merges seed + models.dev + enterprise manifest catalog + pool costs + usage.
func BuildCatalog(m *manifest) []codebuddyModelCatalogRow {
	md := codebuddyModelsDev()
	pool := codebuddyPoolInit()
	logs := codebuddyRequestLogInit()
	usage := logs.ModelUsageStats()
	usageByID := map[string]map[string]any{}
	for _, obj := range usage {
		if id, _ := obj["model"].(string); id != "" {
			usageByID[id] = obj
		}
	}

	disabled := map[string]bool{}
	credits := map[string]string{}
	efforts := map[string][]string{}
	enterprise := map[string]map[string]any{}
	var enabledIDs []string

	if m != nil {
		for _, id := range m.DisabledModels {
			disabled[strings.ToLower(strings.TrimSpace(id))] = true
		}
		for k, v := range m.ModelCredits {
			credits[strings.ToLower(k)] = v
		}
		for k, v := range m.ModelEfforts {
			efforts[strings.ToLower(k)] = v
		}
		for _, row := range m.ModelCatalog {
			id, _ := row["id"].(string)
			if id == "" {
				continue
			}
			enterprise[strings.ToLower(id)] = row
		}
		// Union of catalog ids: enterprise + default list + upstream modelIds.
		seen := map[string]bool{}
		add := func(id string) {
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				return
			}
			seen[id] = true
			enabledIDs = append(enabledIDs, id)
		}
		for id := range enterprise {
			add(id)
		}
		for _, u := range m.CodebuddyUpstreams {
			for _, id := range u.ModelIDs {
				add(id)
			}
		}
		for _, id := range codebuddyDefaultModelIDs {
			add(id)
		}
		sort.Strings(enabledIDs)
	} else {
		enabledIDs = append(enabledIDs, codebuddyDefaultModelIDs...)
	}

	rows := make([]codebuddyModelCatalogRow, 0, len(enabledIDs))
	for _, id := range enabledIDs {
		lower := strings.ToLower(id)
		row := codebuddyModelCatalogRow{ID: id, CLI: true, Name: id}

		if seed, ok := codebuddyModelSeed[lower]; ok {
			row.ContextLength = seed["context_length"]
			row.MaxOutputTokens = seed["max_output_tokens"]
			row.Source = "seed"
		}
		if ctx, out, ok := md.lookup(lower); ok {
			if ctx > 0 {
				row.ContextLength = ctx
				row.Source = "models.dev"
			}
			if out > 0 {
				row.MaxOutputTokens = out
			}
		}
		if ent, ok := enterprise[lower]; ok {
			if name, _ := ent["name"].(string); name != "" {
				row.Name = name
			}
			if v, ok := toFloat64(ent["contextLength"]); ok && v > 0 {
				row.ContextLength = int64(v)
				row.Source = "enterprise"
			}
			if v, ok := toFloat64(ent["maxOutputTokens"]); ok && v > 0 {
				row.MaxOutputTokens = int64(v)
			}
			if s, _ := ent["credits"].(string); s != "" {
				row.Credits = s
			}
			if arr, ok := ent["efforts"].([]any); ok {
				for _, a := range arr {
					if s, ok := a.(string); ok && s != "" {
						row.Efforts = append(row.Efforts, s)
					}
				}
			}
			if b, ok := ent["supportsImages"].(bool); ok {
				row.SupportsImages = b
			}
			if b, ok := ent["supportsReasoning"].(bool); ok {
				row.SupportsReason = b
			}
			if b, ok := ent["supportsToolCall"].(bool); ok {
				row.SupportsTools = b
			}
			if b, ok := ent["cli"].(bool); ok {
				row.CLI = b
			}
			if s, _ := ent["description"].(string); s != "" {
				row.Description = s
			}
			if arr, ok := ent["tags"].([]any); ok {
				for _, a := range arr {
					if s, ok := a.(string); ok && s != "" {
						row.Tags = append(row.Tags, s)
					}
				}
			}
			if s, _ := ent["vendor"].(string); s != "" {
				row.Vendor = s
			}
			if b, ok := ent["isDefault"].(bool); ok {
				row.IsDefault = b
			}
			if b, ok := ent["onlyReasoning"].(bool); ok {
				row.OnlyReasoning = b
			}
			if v, ok := toFloat64(ent["maxAllowedSize"]); ok {
				row.MaxAllowedSize = int64(v)
			}
			if v, ok := ent["creditsRate"].(float64); ok {
				row.CreditsRate = v
			}
		}
		if c, ok := credits[lower]; ok && row.Credits == "" {
			row.Credits = c
		}
		if e, ok := efforts[lower]; ok && len(row.Efforts) == 0 {
			row.Efforts = e
		}
		row.CreditsRate = parseCreditsRate(row.Credits)
		if row.ContextLength <= 0 {
			row.ContextLength = codebuddyDefaultContextWindow
		}
		if row.MaxOutputTokens <= 0 {
			row.MaxOutputTokens = 8192
		}
		row.Disabled = disabled[lower]
		if row.Source == "" {
			row.Source = "preset"
		}

		// Pool runtime cost + configured per-account free policy.
		if pool != nil {
			pool.mu.Lock()
			now := time.Now()
			var bestTier *int
			var bestCost *float64
			var samples int
			var last string
			free := false
			saw := false
			var always, night []string
			for _, e := range pool.entries {
				label := e.Label
				if label == "" {
					label = e.ID
				}
				for _, mid := range e.AlwaysFreeModels {
					if strings.EqualFold(mid, id) || strings.EqualFold(mid, lower) {
						always = append(always, label)
						break
					}
				}
				for _, mid := range e.NightOnlyFreeModels {
					if strings.EqualFold(mid, id) || strings.EqualFold(mid, lower) {
						night = append(night, label)
						break
					}
				}
				mc, ok := e.ModelCosts[id]
				if !ok {
					mc, ok = e.ModelCosts[lower]
				}
				if !ok {
					continue
				}
				saw = true
				samples += mc.Samples
				if mc.LastSeen.After(time.Time{}) {
					ts := mc.LastSeen.UTC().Format(time.RFC3339)
					if ts > last {
						last = ts
					}
				}
				tier := 1
				if mc.CostPer1k <= 0 {
					tier = 0
					free = true
				} else {
					tier = 2
				}
				t := tier
				if bestTier == nil || t < *bestTier {
					bestTier = &t
				}
				c := mc.CostPer1k
				if bestCost == nil || c < *bestCost {
					bestCost = &c
				}
			}
			// Configured always-free accounts count as free observation.
			if len(always) > 0 {
				free = true
				t := 0
				if bestTier == nil || t < *bestTier {
					bestTier = &t
				}
				z := 0.0
				if bestCost == nil {
					bestCost = &z
				}
			}
			pool.mu.Unlock()
			row.AccountsAlwaysFree = always
			row.AccountsNightFree = night
			row.FreePolicySummary = summarizeAccountFree(always, night, now)
			if saw || len(always) > 0 {
				row.CostSamples = samples
				row.CostLastSeen = last
				row.CostTier = bestTier
				row.CostPer1k = bestCost
				f := free
				row.FreeObserved = &f
			}
		}

		// Usage aggregate
		if u, ok := usageByID[id]; ok {
			if v, ok := toFloat64(u["requests"]); ok {
				row.Requests = int64(v)
			}
			if v, ok := toFloat64(u["totalTokens"]); ok {
				row.TotalTokens = int64(v)
			}
			if v, ok := toFloat64(u["credit"]); ok {
				row.TotalCredit = v
			}
		} else if u, ok := usageByID[lower]; ok {
			if v, ok := toFloat64(u["requests"]); ok {
				row.Requests = int64(v)
			}
			if v, ok := toFloat64(u["totalTokens"]); ok {
				row.TotalTokens = int64(v)
			}
			if v, ok := toFloat64(u["credit"]); ok {
				row.TotalCredit = v
			}
		}

		// Heuristic free label when catalog credits are x0.00
		if row.FreeObserved == nil && strings.TrimSpace(strings.ToLower(row.Credits)) != "" {
			if row.CreditsRate == 0 {
				t := true
				row.FreeObserved = &t
			} else {
				t := false
				row.FreeObserved = &t
			}
		}

		rows = append(rows, row)
	}
	return rows
}

// CostExploreConfig from env + defaults (workbuddy2api-aligned surface).
func CostExploreConfig() map[string]any {
	interval := "30m"
	enabled := true
	if v := strings.TrimSpace(os.Getenv("COCKPIT_CODEBUDDY_COST_EXPLORE")); v != "" {
		if v == "0" || strings.EqualFold(v, "off") || strings.EqualFold(v, "false") {
			enabled = false
			interval = "0"
		} else {
			interval = v
		}
	}
	return map[string]any{
		"enabled":          enabled,
		"interval":         interval,
		"defaultInterval":  "30m",
		"env":              "COCKPIT_CODEBUDDY_COST_EXPLORE",
		"note":             "tier0 垄断且存在 tier1 时，窗口内搭车改道探索未知号；0=关闭",
	}
}
