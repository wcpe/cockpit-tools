package main

import (
	"strings"
	"time"
)

// codebuddy_night_free.go — night free-window model policy (aligned with
// WorkBuddy 夜猫子活动 23:00–08:00 CST).
//
// When enabled, models listed in manifest.NightFreeModels are only callable
// inside the night window. Outside the window the gateway rejects chat and
// omits them from /v1/models so clients cannot accidentally burn credits.

// Night window defaults (CST / UTC+8), matching activity night cat hours.
const (
	codebuddyNightStartHour = 23
	codebuddyNightEndHour   = 8
)

type codebuddyNightFreePolicy struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models"`
	// StartHour/EndHour in CST 0-23. Default 23→8 (wraps midnight).
	StartHour int `json:"startHour"`
	EndHour   int `json:"endHour"`
	// Window wrap is implicit when start > end.
}

func codebuddyNightFreeFromManifest(m *manifest) codebuddyNightFreePolicy {
	p := codebuddyNightFreePolicy{
		Enabled:   false,
		StartHour: codebuddyNightStartHour,
		EndHour:   codebuddyNightEndHour,
	}
	if m == nil {
		return p
	}
	p.Enabled = m.NightFreeEnabled
	p.Models = normalizeCodebuddyModelList(m.NightFreeModels)
	if m.NightFreeStartHour != nil {
		h := *m.NightFreeStartHour
		if h >= 0 && h <= 23 {
			p.StartHour = h
		}
	}
	if m.NightFreeEndHour != nil {
		h := *m.NightFreeEndHour
		if h >= 0 && h <= 23 {
			p.EndHour = h
		}
	}
	return p
}

func normalizeCodebuddyModelList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	return out
}

// cstNow returns current time in Asia/Shanghai (UTC+8, no DST).
func codebuddyCSTNow(now time.Time) time.Time {
	return now.UTC().Add(8 * time.Hour)
}

// InNightWindow reports whether now (any zone) falls in the CST night window.
// Window math does not depend on the model list; policy-off short-circuits only
// when the master switch is disabled.
func (p codebuddyNightFreePolicy) InNightWindow(now time.Time) bool {
	if !p.Enabled {
		return true // policy off → no extra restriction anywhere
	}
	cst := codebuddyCSTNow(now)
	hour := cst.Hour()
	start, end := p.StartHour, p.EndHour
	if start < 0 || start > 23 {
		start = codebuddyNightStartHour
	}
	if end < 0 || end > 23 {
		end = codebuddyNightEndHour
	}
	if start == end {
		return true // degenerate 24h
	}
	if start < end {
		return hour >= start && hour < end
	}
	// wrap: e.g. 23 → 8
	return hour >= start || hour < end
}

// IsNightFreeModel reports whether model is under night-window restriction.
func (p codebuddyNightFreePolicy) IsNightFreeModel(model string) bool {
	if !p.Enabled {
		return false
	}
	model = strings.TrimSpace(model)
	lower := strings.ToLower(model)
	for _, id := range p.Models {
		if strings.EqualFold(id, model) || strings.ToLower(id) == lower {
			return true
		}
	}
	return false
}

// AllowModel returns (allowed, reason). Outside window night-free models are denied.
func (p codebuddyNightFreePolicy) AllowModel(model string, now time.Time) (bool, string) {
	if !p.IsNightFreeModel(model) {
		return true, ""
	}
	if p.InNightWindow(now) {
		return true, ""
	}
	return false, "model is night-free only (" +
		hourLabel(p.StartHour) + "-" + hourLabel(p.EndHour) + " CST); request outside window to avoid billing"
}

func hourLabel(h int) string {
	if h < 0 || h > 23 {
		return "?"
	}
	return strings.TrimSpace(time.Date(2000, 1, 1, h, 0, 0, 0, time.UTC).Format("15"))
}

// Status payload for UI / catalog.
func (p codebuddyNightFreePolicy) Status(now time.Time) map[string]any {
	in := p.InNightWindow(now)
	cst := codebuddyCSTNow(now)
	return map[string]any{
		"enabled":     p.Enabled,
		"models":      p.Models,
		"startHour":   p.StartHour,
		"endHour":     p.EndHour,
		"window":      hourLabel(p.StartHour) + "-" + hourLabel(p.EndHour) + " CST",
		"inWindow":    in,
		"nowCST":      cst.Format("2006-01-02 15:04:05"),
		"behavior":    "窗外拒绝 nightFree 模型请求，避免额外计费",
	}
}

// FilterCatalogModels removes night-free models from the public model list when
// outside the window (so clients cannot select them).
func (p codebuddyNightFreePolicy) FilterCatalogModels(ids []string, now time.Time) []string {
	if !p.Enabled || p.InNightWindow(now) || len(p.Models) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if p.IsNightFreeModel(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}
