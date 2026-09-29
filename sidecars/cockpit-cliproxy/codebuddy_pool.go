package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// codebuddy_pool.go ports workbuddy2api pool selection into the CodeBuddy gateway:
// three-factor weighted random (credits ×10 + expiring ×8 + idle),
// costTier free-first, top-5 shortlist, anti-herd pick gap, in-flight lease,
// model cost ledger, WAF soft cooldown + IP fail-fast, degrade, session-dead.
//
// Credits enter from the Rust manifest when available; otherwise weight starts
// neutral and is refined by runtime usage.credit observations.

const (
	codebuddyPickTopK         = 5
	codebuddyMinPickGap       = 100 * time.Millisecond
	codebuddyIdleWeightPerHour = 0.5
	codebuddyIdleWeightMax     = 5.0
	codebuddyExpiringWeight    = 8.0
	codebuddyModelCostTTL      = 6 * time.Hour
	codebuddyDefaultMaxInFlight = 2
	codebuddyDegradeThreshold  = 5
	codebuddyDegradeCooldown   = 10 * time.Minute
	codebuddyDegradeCooldownMax = 2 * time.Hour
	codebuddySessionDeadThreshold = 3
	codebuddyWafCooldownBase   = 60 * time.Second
	codebuddyWafCooldownMax    = 30 * time.Minute
	codebuddyCostExploreInterval = 30 * time.Minute
)

var (
	codebuddyPoolOnce sync.Once
	codebuddyPoolInst *codebuddyPool
)

type codebuddyModelCost struct {
	CostPer1k float64   `json:"costPer1k"`
	LastSeen  time.Time `json:"lastSeen"`
	Samples   int       `json:"samples"`
}

type codebuddyPoolEntry struct {
	ID                string `json:"id"`
	UID               string `json:"uid,omitempty"`
	Label             string `json:"label,omitempty"`
	Realm             string `json:"realm,omitempty"`
	Credits           int64  `json:"credits"`
	CreditsExpiring   int64  `json:"creditsExpiring"`
	CreditsKnown      bool   `json:"creditsKnown,omitempty"`
	InFlight          int    `json:"inFlight"`
	UsedSeq           uint64 `json:"usedSeq"`
	BreakerFailures   int    `json:"breakerFailures"`
	DegradeFails      int    `json:"degradeFails"`
	SessionDeadFails  int    `json:"sessionDeadFails"`
	WafStreak         int    `json:"wafStreak"`
	MaxInFlight       int    `json:"maxInFlight,omitempty"`
	Disabled          bool   `json:"disabled,omitempty"`
	DisableReason     string `json:"disableReason,omitempty"`

	LastUsed          time.Time `json:"lastUsed,omitempty"`
	DegradeUntil      time.Time `json:"degradeUntil,omitempty"`
	WafUntil          time.Time `json:"wafUntil,omitempty"`
	SessionDeadUntil  time.Time `json:"sessionDeadUntil,omitempty"`
	ModelCooldowns    map[string]time.Time `json:"modelCooldowns,omitempty"`
	ModelCosts        map[string]codebuddyModelCost `json:"modelCosts,omitempty"`
	// Configured entitlements from manifest (survive sync).
	AlwaysFreeModels     []string `json:"alwaysFreeModels,omitempty"`
	NightOnlyFreeModels  []string `json:"nightOnlyFreeModels,omitempty"`
}

type codebuddyPoolState struct {
	Version         int                              `json:"version"`
	Accounts        map[string]*codebuddyPoolEntry   `json:"accounts"`
	ExploreLast     map[string]time.Time             `json:"exploreLast,omitempty"`
	CostExploreEvents int64                          `json:"costExploreEvents,omitempty"`
	PickSeq         uint64                           `json:"pickSeq,omitempty"`
}

type codebuddyPool struct {
	mu        sync.Mutex
	path      string
	entries   map[string]*codebuddyPoolEntry
	exploreLast map[string]time.Time
	costExploreEvents int64
	pickSeq   uint64
	// IP-level WAF fail-fast (chat rotate stop).
	wafIPHits map[string]time.Time
	wafIPUntil time.Time
}

func codebuddyPoolPath() string {
	if dir := strings.TrimSpace(os.Getenv("COCKPIT_CODEBUDDY_STATE_DIR")); dir != "" {
		return filepath.Join(dir, "codebuddy_pool_state.json")
	}
	if dir := strings.TrimSpace(os.Getenv("COCKPIT_TOOLS_DATA_DIR")); dir != "" {
		return filepath.Join(dir, "codebuddy_pool_state.json")
	}
	// Reuse governance directory naming for consistency with codebuddyGovernancePath.
	home, _ := os.UserHomeDir()
	if home == "" {
		return "codebuddy_pool_state.json"
	}
	return filepath.Join(home, ".antigravity_cockpit", "codebuddy_pool_state.json")
}

func codebuddyPoolInit() *codebuddyPool {
	codebuddyPoolOnce.Do(func() {
		p := &codebuddyPool{
			path:        codebuddyPoolPath(),
			entries:     map[string]*codebuddyPoolEntry{},
			exploreLast: map[string]time.Time{},
			wafIPHits:   map[string]time.Time{},
		}
		p.load()
		codebuddyPoolInst = p
	})
	return codebuddyPoolInst
}

func (p *codebuddyPool) load() {
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return
	}
	var st codebuddyPoolState
	if err := json.Unmarshal(raw, &st); err != nil {
		return
	}
	if st.Accounts != nil {
		p.entries = st.Accounts
	}
	if st.ExploreLast != nil {
		p.exploreLast = st.ExploreLast
	}
	p.costExploreEvents = st.CostExploreEvents
	p.pickSeq = st.PickSeq
}

func (p *codebuddyPool) persistLocked() {
	if p.path == "" {
		return
	}
	st := codebuddyPoolState{
		Version:           1,
		Accounts:          p.entries,
		ExploreLast:       p.exploreLast,
		CostExploreEvents: p.costExploreEvents,
		PickSeq:           p.pickSeq,
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p.path), 0o755)
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.path)
}

func (p *codebuddyPool) entryLocked(id string) *codebuddyPoolEntry {
	if p.entries == nil {
		p.entries = map[string]*codebuddyPoolEntry{}
	}
	e := p.entries[id]
	if e == nil {
		e = &codebuddyPoolEntry{
			ID:             id,
			MaxInFlight:    codebuddyDefaultMaxInFlight,
			ModelCooldowns: map[string]time.Time{},
			ModelCosts:     map[string]codebuddyModelCost{},
		}
		p.entries[id] = e
	}
	if e.ModelCooldowns == nil {
		e.ModelCooldowns = map[string]time.Time{}
	}
	if e.ModelCosts == nil {
		e.ModelCosts = map[string]codebuddyModelCost{}
	}
	if e.MaxInFlight <= 0 {
		e.MaxInFlight = codebuddyDefaultMaxInFlight
	}
	return e
}

// SyncFromManifest refreshes identity/credits/realm from the generated manifest.
func (p *codebuddyPool) SyncFromManifest(upstreams []codebuddyUpstreamSpec) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range upstreams {
		u := &upstreams[i]
		if u.ID == "" {
			continue
		}
		e := p.entryLocked(u.ID)
		e.UID = u.UID
		e.Label = u.Label
		if r := strings.TrimSpace(u.Realm); r != "" {
			e.Realm = strings.ToLower(r)
		} else {
			e.Realm = codebuddyInferRealm(u)
		}
		if u.Credits != nil {
			e.Credits = *u.Credits
			e.CreditsKnown = true
		}
		if u.CreditsExpiring != nil {
			e.CreditsExpiring = *u.CreditsExpiring
		}
		e.AlwaysFreeModels = normalizeCodebuddyModelList(u.AlwaysFreeModels)
		e.NightOnlyFreeModels = normalizeCodebuddyModelList(u.NightOnlyFreeModels)
		e.Disabled = u.Disabled
		if e.InFlight < 0 {
			e.InFlight = 0
		}
	}
	p.persistLocked()
}

// SyncFromPointers is used when only a slice of pointers is available.
func (p *codebuddyPool) SyncFromPointers(upstreams []*codebuddyUpstreamSpec) {
	if p == nil {
		return
	}
	specs := make([]codebuddyUpstreamSpec, 0, len(upstreams))
	for _, u := range upstreams {
		if u != nil {
			specs = append(specs, *u)
		}
	}
	p.SyncFromManifest(specs)
}

func codebuddyInferRealm(u *codebuddyUpstreamSpec) string {
	base := strings.ToLower(u.BaseURL)
	switch {
	case strings.Contains(base, "workbuddy.ai"):
		return "global"
	case strings.Contains(base, "copilot.tencent.com"),
		strings.Contains(base, "codebuddy.cn"),
		strings.Contains(base, "workbuddy.cn"):
		return "cn"
	default:
		return "cn"
	}
}

// Available reports whether the account can take a request for model.
func (p *codebuddyPool) Available(id, model string) (bool, string) {
	if p == nil {
		return true, ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.availableLocked(id, model, time.Now())
}

func (p *codebuddyPool) availableLocked(id, model string, now time.Time) (bool, string) {
	e := p.entries[id]
	if e == nil {
		return true, ""
	}
	if e.Disabled {
		return false, "account disabled"
	}
	if !e.DegradeUntil.IsZero() && now.Before(e.DegradeUntil) {
		return false, "degrade cooling"
	}
	if !e.WafUntil.IsZero() && now.Before(e.WafUntil) {
		return false, "waf cooling"
	}
	if !e.SessionDeadUntil.IsZero() && now.Before(e.SessionDeadUntil) {
		return false, "session dead cooling"
	}
	if e.InFlight >= e.MaxInFlight && e.MaxInFlight > 0 {
		return false, "in-flight full"
	}
	if model != "" {
		if until, ok := e.ModelCooldowns[model]; ok && now.Before(until) {
			return false, "model cooling"
		}
	}
	// Governance soft/hard cooldown still applies via codebuddyGovernance.available.
	return true, ""
}

func (p *codebuddyPool) pruneModelCostsLocked(e *codebuddyPoolEntry, now time.Time) {
	for m, mc := range e.ModelCosts {
		if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > codebuddyModelCostTTL {
			delete(e.ModelCosts, m)
		}
	}
	for m, until := range e.ModelCooldowns {
		if !now.Before(until) {
			delete(e.ModelCooldowns, m)
		}
	}
}

func (p *codebuddyPool) costTierLocked(e *codebuddyPoolEntry, model string) (int, float64) {
	// Configured account entitlement first (hy4-preview 全天免费 / 仅夜间免费).
	if tier, ok := p.accountFreeTierAt(e, model, time.Now()); ok {
		return tier, 0
	}
	mc, ok := e.ModelCosts[model]
	if !ok || mc.LastSeen.IsZero() || time.Since(mc.LastSeen) > codebuddyModelCostTTL {
		return 1, 0 // unknown
	}
	if mc.CostPer1k <= 0 {
		return 0, 0 // free
	}
	return 2, mc.CostPer1k
}

// accountFreeTierAt maps configured free policy to costTier at a given time:
//   - always free → tier 0 (any hour)
//   - night-only free → tier 0 inside 23:00–08:00 CST, else tier 2 (paid)
func (p *codebuddyPool) accountFreeTierAt(e *codebuddyPoolEntry, model string, now time.Time) (int, bool) {
	if e == nil || model == "" {
		return 0, false
	}
	for _, id := range e.AlwaysFreeModels {
		if strings.EqualFold(id, model) {
			return 0, true
		}
	}
	for _, id := range e.NightOnlyFreeModels {
		if !strings.EqualFold(id, model) {
			continue
		}
		if nightPolicyInWindow(now) {
			return 0, true
		}
		return 2, true
	}
	return 0, false
}

// nightPolicyInWindow uses global night window defaults (23–8 CST) for
// account-level night-only free entitlements.
func nightPolicyInWindow(now time.Time) bool {
	p := codebuddyNightFreePolicy{
		Enabled:   true,
		StartHour: codebuddyNightStartHour,
		EndHour:   codebuddyNightEndHour,
	}
	return p.InNightWindow(now)
}

// AccountFreeLabel human-readable free type for UI.
func AccountFreeLabel(e *codebuddyPoolEntry, model string, now time.Time) string {
	if e == nil {
		return ""
	}
	for _, id := range e.AlwaysFreeModels {
		if strings.EqualFold(id, model) {
			return "always_free"
		}
	}
	for _, id := range e.NightOnlyFreeModels {
		if !strings.EqualFold(id, model) {
			continue
		}
		if nightPolicyInWindow(now) {
			return "night_free_in_window"
		}
		return "night_free_out_window"
	}
	return ""
}

func (p *codebuddyPool) weightOfLocked(e *codebuddyPoolEntry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	if maxCredits > 0 && e.Credits > 0 {
		w += float64(e.Credits) / float64(maxCredits) * 10
	}
	if e.Credits > 0 && e.CreditsExpiring > 0 {
		w += float64(e.CreditsExpiring) / float64(e.Credits) * codebuddyExpiringWeight
	}
	if e.LastUsed.IsZero() {
		w += codebuddyIdleWeightMax
	} else {
		hours := now.Sub(e.LastUsed).Hours()
		idle := hours * codebuddyIdleWeightPerHour
		if idle > codebuddyIdleWeightMax {
			idle = codebuddyIdleWeightMax
		}
		if idle < 0 {
			idle = 0
		}
		w += idle
	}
	return w
}

type codebuddyPickCandidate struct {
	id     string
	weight float64
	tier   int
	cost1k float64
}

// PickOrder returns a selection order for the given upstreams under pool policy.
// Sticky/realm/catalog filters happen before this call; the pool reorders healthy
// candidates by costTier + weighted random shortlist, then appends the rest as
// rotation fallbacks.
func (p *codebuddyPool) PickOrder(upstreams []*codebuddyUpstreamSpec, model, realm string, routing string) []*codebuddyUpstreamSpec {
	if p == nil || len(upstreams) <= 1 {
		return upstreams
	}
	now := time.Now()
	model = strings.TrimSpace(model)
	realm = strings.ToLower(strings.TrimSpace(realm))
	// Model realm prefix: "cn:model" / "global:model"
	if strings.Contains(model, ":") {
		parts := strings.SplitN(model, ":", 2)
		if pref := strings.ToLower(parts[0]); pref == "cn" || pref == "global" || pref == "intl" {
			if pref == "intl" {
				pref = "global"
			}
			realm = pref
			model = parts[1]
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	type scored struct {
		u    *codebuddyUpstreamSpec
		w    float64
		tier int
		cost float64
		ok   bool
	}
	var healthy []scored
	var blocked []*codebuddyUpstreamSpec
	var maxCredits int64
	for _, u := range upstreams {
		if u == nil {
			continue
		}
		e := p.entryLocked(u.ID)
		p.pruneModelCostsLocked(e, now)
		if realm != "" && e.Realm != "" && e.Realm != realm {
			blocked = append(blocked, u)
			continue
		}
		ok, _ := p.availableLocked(u.ID, model, now)
		// Also honor pick-gap anti-herd for selection order head.
		if ok && !e.LastUsed.IsZero() && now.Sub(e.LastUsed) < codebuddyMinPickGap {
			// Still eligible as fallback later; deprioritize for this pick.
			ok = false
		}
		if !ok {
			blocked = append(blocked, u)
			continue
		}
		tier, cost := p.costTierLocked(e, model)
		if e.Credits > maxCredits {
			maxCredits = e.Credits
		}
		healthy = append(healthy, scored{u: u, tier: tier, cost: cost, ok: true})
	}
	if len(healthy) == 0 {
		// Everything cooling — keep original order as fallthrough.
		return upstreams
	}
	for i := range healthy {
		e := p.entryLocked(healthy[i].u.ID)
		healthy[i].w = p.weightOfLocked(e, maxCredits, now)
	}

	// Cost explore: if bestTier==0 and some tier1 exists and window elapsed.
	bestTier := 2
	for _, h := range healthy {
		if h.tier < bestTier {
			bestTier = h.tier
		}
	}
	explored := false
	interval := codebuddyCostExploreInterval
	if v := strings.TrimSpace(os.Getenv("COCKPIT_CODEBUDDY_COST_EXPLORE")); v != "" {
		if v == "0" || strings.EqualFold(v, "off") {
			interval = 0
		}
	}
	if interval > 0 && bestTier == 0 && model != "" && realm != "" || (interval > 0 && bestTier == 0 && model != "") {
		key := realm + "\x1f" + model
		hasTier1 := false
		for _, h := range healthy {
			if h.tier == 1 {
				hasTier1 = true
				break
			}
		}
		if hasTier1 && now.Sub(p.exploreLast[key]) >= interval {
			p.exploreLast[key] = now
			p.costExploreEvents++
			for i := range healthy {
				if healthy[i].tier == 1 {
					healthy[i].tier = -1 // explore boost this round
				}
			}
			explored = true
		}
	}

	// Prefer lowest tier among alive candidates (explore uses -1).
	minTier := 2
	for _, h := range healthy {
		t := h.tier
		if t < 0 {
			t = 0
		}
		if t < minTier {
			minTier = t
		}
	}
	var eligible []scored
	for _, h := range healthy {
		t := h.tier
		if t < 0 {
			t = 0
		}
		if t == minTier {
			eligible = append(eligible, h)
		}
	}
	if len(eligible) == 0 {
		eligible = healthy
	}

	// Sort: free first by weight desc; paid by cost asc then weight.
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].cost != eligible[j].cost {
			return eligible[i].cost < eligible[j].cost
		}
		if eligible[i].w != eligible[j].w {
			return eligible[i].w > eligible[j].w
		}
		return eligible[i].u.ID < eligible[j].u.ID
	})

	// Top-K shortlist + weighted lottery inside shortlist.
	k := codebuddyPickTopK
	if k > len(eligible) {
		k = len(eligible)
	}
	shortlist := eligible[:k]
	// Shuffle equal weights to avoid herd on first account.
	rand.Shuffle(len(shortlist), func(i, j int) {
		if shortlist[i].w == shortlist[j].w && shortlist[i].cost == shortlist[j].cost {
			shortlist[i], shortlist[j] = shortlist[j], shortlist[i]
		}
	})
	const scale = 1_000_000
	var total int64
	weights := make([]int64, len(shortlist))
	for i, h := range shortlist {
		wi := int64(h.w*scale + 0.5)
		if wi < 1 {
			wi = 1
		}
		weights[i] = wi
		total += wi
	}
	pickIdx := len(shortlist) - 1
	if total > 0 {
		r := rand.Int63n(total)
		var acc int64
		for i := range shortlist {
			acc += weights[i]
			if r < acc {
				pickIdx = i
				break
			}
		}
	}
	chosen := shortlist[pickIdx].u.ID
	if explored {
		log.Printf("[codebuddy-pool] cost explore model=%s realm=%q acct=%s", model, realm, chosen)
	}

	// Build order: chosen, other eligible, then blocked (rotation fallback).
	ordered := make([]*codebuddyUpstreamSpec, 0, len(upstreams))
	ordered = append(ordered, chosenUpstream(upstreams, chosen))
	for _, h := range eligible {
		if h.u.ID == chosen {
			continue
		}
		ordered = append(ordered, h.u)
	}
	// Remaining healthy that were not eligible (higher tier) still better than blocked.
	for _, h := range healthy {
		found := false
		for _, o := range ordered {
			if o != nil && o.ID == h.u.ID {
				found = true
				break
			}
		}
		if !found {
			ordered = append(ordered, h.u)
		}
	}
	ordered = append(ordered, blocked...)
	// Preserve routing strategy intent: if random and multiple equal, rotate start.
	if strings.EqualFold(strings.TrimSpace(routing), "random") && len(ordered) > 1 {
		// Keep pool-chosen head; do not re-randomize over pool intelligence.
	}
	return ordered
}

func chosenUpstream(all []*codebuddyUpstreamSpec, id string) *codebuddyUpstreamSpec {
	for _, u := range all {
		if u != nil && u.ID == id {
			return u
		}
	}
	if len(all) > 0 {
		return all[0]
	}
	return nil
}

// Acquire marks an in-flight request on the account.
func (p *codebuddyPool) Acquire(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(id)
	e.InFlight++
	p.persistLocked()
}

// Release finishes an in-flight request and stamps lastUsed.
func (p *codebuddyPool) Release(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(id)
	if e.InFlight > 0 {
		e.InFlight--
	}
	e.LastUsed = time.Now()
	p.pickSeq++
	e.UsedSeq = p.pickSeq
	p.persistLocked()
}

// NoteSuccess clears breaker/degrade/waf streaks for the account.
func (p *codebuddyPool) NoteSuccess(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(id)
	e.BreakerFailures = 0
	e.DegradeFails = 0
	e.WafStreak = 0
	e.SessionDeadFails = 0
	p.persistLocked()
}

// NoteModelCost records usage.credit observation for (account, model).
// credit<=0 marks free (tier 0); positive updates EMA and deducts credits.
func (p *codebuddyPool) NoteModelCost(id, model string, credit float64, tokens int) {
	if p == nil || id == "" || model == "" {
		return
	}
	if tokens <= 0 && credit == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(id)
	p.pruneModelCostsLocked(e, time.Now())
	per1k := 0.0
	if tokens > 0 {
		per1k = credit / (float64(tokens) / 1000.0)
	} else {
		per1k = credit
	}
	prev, ok := e.ModelCosts[model]
	if !ok {
		e.ModelCosts[model] = codebuddyModelCost{CostPer1k: per1k, LastSeen: time.Now(), Samples: 1}
	} else {
		if prev.CostPer1k <= 0 && per1k > 0 {
			log.Printf("[codebuddy-pool] model %s on %s: free tier ended, now %.3f credits/1k", model, id, per1k)
		}
		const alpha = 0.3
		e.ModelCosts[model] = codebuddyModelCost{
			CostPer1k: prev.CostPer1k*(1-alpha) + per1k*alpha,
			LastSeen:  time.Now(),
			Samples:   prev.Samples + 1,
		}
	}
	if credit > 0 && e.CreditsKnown {
		e.Credits -= int64(credit + 0.5)
		if e.Credits < 0 {
			e.Credits = 0
		}
		if e.CreditsExpiring > 0 {
			e.CreditsExpiring -= int64(credit + 0.5)
			if e.CreditsExpiring < 0 {
				e.CreditsExpiring = 0
			}
		}
	}
	p.persistLocked()
}

// NoteFailure applies degrade / breaker accounting.
func (p *codebuddyPool) NoteFailure(id, reason string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(id)
	e.DegradeFails++
	e.BreakerFailures++
	if e.DegradeFails >= codebuddyDegradeThreshold {
		shift := e.DegradeFails - codebuddyDegradeThreshold
		if shift > 4 {
			shift = 4
		}
		d := codebuddyDegradeCooldown << shift
		if d > codebuddyDegradeCooldownMax {
			d = codebuddyDegradeCooldownMax
		}
		e.DegradeUntil = time.Now().Add(d)
	}
	p.persistLocked()
}

// NoteWaf records a WAF 403 soft cooldown and may activate IP-level fail-fast.
func (p *codebuddyPool) NoteWaf(id string) (ipBlocked bool) {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if id != "" {
		e := p.entryLocked(id)
		e.WafStreak++
		shift := e.WafStreak - 1
		if shift > 6 {
			shift = 6
		}
		d := codebuddyWafCooldownBase << shift
		// ±25% jitter
		jitter := time.Duration(rand.Int63n(int64(d)/2+1) - int64(d)/4)
		d += jitter
		if d > codebuddyWafCooldownMax {
			d = codebuddyWafCooldownMax
		}
		if d < codebuddyWafCooldownBase/2 {
			d = codebuddyWafCooldownBase / 2
		}
		e.WafUntil = now.Add(d)
	}
	// IP gate: ≥2 distinct accounts within 60s.
	if now.Before(p.wafIPUntil) {
		p.persistLocked()
		return true
	}
	if p.wafIPHits == nil {
		p.wafIPHits = map[string]time.Time{}
	}
	uid := id
	if uid == "" {
		uid = "anon"
	}
	p.wafIPHits[uid] = now
	for k, t := range p.wafIPHits {
		if now.Sub(t) > time.Minute {
			delete(p.wafIPHits, k)
		}
	}
	if len(p.wafIPHits) >= 2 {
		p.wafIPUntil = now.Add(time.Minute)
		log.Printf("WARN: [codebuddy-pool] WAF IP-level block: %d accounts in 60s, fail-fast until %s",
			len(p.wafIPHits), p.wafIPUntil.Format(time.RFC3339))
		p.wafIPHits = map[string]time.Time{}
		p.persistLocked()
		return true
	}
	p.persistLocked()
	return false
}

// WafIPActive reports whether IP-level WAF fail-fast is active.
func (p *codebuddyPool) WafIPActive() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.wafIPUntil)
}

// NoteSessionDead counts 12153 and disables after threshold.
func (p *codebuddyPool) NoteSessionDead(id string) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(id)
	e.SessionDeadFails++
	if e.SessionDeadFails >= codebuddySessionDeadThreshold {
		e.Disabled = true
		e.DisableReason = "12153 session dead"
		e.SessionDeadUntil = time.Now().Add(24 * time.Hour)
		log.Printf("[codebuddy-pool] account %s disabled: session dead", id)
	}
	p.persistLocked()
}

// SetCredits updates known balance (from manifest/check-in).
func (p *codebuddyPool) SetCredits(id string, credits, expiring int64) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(id)
	e.Credits = credits
	e.CreditsExpiring = expiring
	e.CreditsKnown = true
	// Balance recovery unfreezes credit-related disable/degrade.
	if credits > 0 {
		e.Disabled = false
		e.DisableReason = ""
		if !e.DegradeUntil.IsZero() {
			e.DegradeUntil = time.Time{}
		}
	}
	p.persistLocked()
}

// Ledger exposes pool runtime for /status and the desktop UI.
func (p *codebuddyPool) Ledger() map[string]any {
	if p == nil {
		return map[string]any{"accounts": []any{}, "costExploreEvents": 0}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	accounts := make([]map[string]any, 0, len(p.entries))
	for _, e := range p.entries {
		p.pruneModelCostsLocked(e, now)
		costs := map[string]any{}
		for m, mc := range e.ModelCosts {
			tier := 1
			if mc.CostPer1k <= 0 {
				tier = 0
			} else {
				tier = 2
			}
			costs[m] = map[string]any{
				"costPer1k": mc.CostPer1k,
				"tier":      tier,
				"samples":   mc.Samples,
				"lastSeen":  mc.LastSeen.UTC().Format(time.RFC3339),
			}
		}
		coolModels := []map[string]any{}
		for m, until := range e.ModelCooldowns {
			if now.Before(until) {
				coolModels = append(coolModels, map[string]any{
					"model": m,
					"until": until.UTC().Format(time.RFC3339),
				})
			}
		}
		accounts = append(accounts, map[string]any{
			"id":               e.ID,
			"uid":              e.UID,
			"label":            e.Label,
			"realm":            e.Realm,
			"credits":          e.Credits,
			"creditsExpiring":  e.CreditsExpiring,
			"creditsKnown":     e.CreditsKnown,
			"inFlight":         e.InFlight,
			"maxInFlight":      e.MaxInFlight,
			"disabled":         e.Disabled,
			"disableReason":    e.DisableReason,
			"breakerFailures":  e.BreakerFailures,
			"degradeFails":     e.DegradeFails,
			"sessionDeadFails": e.SessionDeadFails,
			"wafStreak":        e.WafStreak,
			"lastUsed":         formatOptionalTime(e.LastUsed),
			"degradeUntil":     formatOptionalTime(e.DegradeUntil),
			"wafUntil":         formatOptionalTime(e.WafUntil),
			"modelCosts":       costs,
			"modelCooldowns":   coolModels,
			"alwaysFreeModels":     e.AlwaysFreeModels,
			"nightOnlyFreeModels":  e.NightOnlyFreeModels,
			"weight":           p.weightOfLocked(e, maxCreditsOf(p.entries), now),
		})
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i]["id"].(string) < accounts[j]["id"].(string)
	})
	explore := map[string]any{}
	for k, t := range p.exploreLast {
		explore[strings.ReplaceAll(k, "\x1f", "|")] = t.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"accounts":           accounts,
		"costExploreEvents":  p.costExploreEvents,
		"costExploreLast":    explore,
		"wafIpActive":        now.Before(p.wafIPUntil),
		"wafIpUntil":         formatOptionalTime(p.wafIPUntil),
		"pickSeq":            p.pickSeq,
	}
}

func maxCreditsOf(entries map[string]*codebuddyPoolEntry) int64 {
	var mx int64
	for _, e := range entries {
		if e.Credits > mx {
			mx = e.Credits
		}
	}
	return mx
}
