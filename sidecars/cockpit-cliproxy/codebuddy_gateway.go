package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// codebuddy_gateway.go serves CodeBuddy / CodeBuddy CN / WorkBuddy subscription
// accounts as an OpenAI-compatible endpoint.
//
// Upstream contract (reverse engineered and verified against copilot.tencent.com):
//
//	POST {baseUrl}/v2/chat/completions
//	Authorization: Bearer <JWT>
//	Content-Type: application/json;charset=UTF-8
//	User-Agent: <browser UA, the gateway rejects unknown clients>
//	X-Agent-Intent: craft          (optional)
//
// The response is an OpenAI-shaped SSE stream. Two upstream constraints drive
// this adapter:
//
//  1. `stream: false` is rejected ("Non-stream chat request is currently not
//     supported"), so upstream requests are always streamed and a single
//     `chat.completion` object is synthesized for non-streaming clients.
//  2. The response `model` is the resolved server-side endpoint id, so it is
//     rewritten back to the client-requested model.
const codebuddyHealthPath = "/v2/plugin/accounts"

// codebuddyDefaultModelIDs is the published catalog used when an account does
// not carry its own list. Mirrors the `cli` agent entry of
// `GET /console/enterprises/personal/models`; Cockpit normally overrides this
// with a live fetch, so treat it as a fallback only.
var codebuddyDefaultModelIDs = []string{
	"auto",
	"hy4-preview",
	"hy3",
	"hy3-x",
	"deepseek-v4.1-flash",
	"glm-5.3",
	"glm-5.3-flash",
	"glm-5.2",
	"glm-5.1",
	"glm-5v-turbo",
	"kimi-k3-1",
	"kimi-k2.8-preview",
	"kimi-k2.7",
	"kimi-k2.6",
	"minimax-m3",
	"deepseek-v4-pro",
}

var codebuddyRotationCounter atomic.Uint64

func isCodebuddyAPIKey(spec *apiKeySpec) bool {
	return spec != nil && normalizeUpstreamKind(spec.UpstreamKind) == codebuddyUpstreamKind
}

// codebuddyUpstreamsForAPIKey returns the usable upstreams for a client key in
// selection order. An api key may scope itself with accountIds; an empty scope
// means every configured upstream.
func codebuddyUpstreamsForAPIKey(m *manifest, spec *apiKeySpec) []*codebuddyUpstreamSpec {
	if m == nil || spec == nil {
		return nil
	}
	selected := make([]*codebuddyUpstreamSpec, 0, len(m.CodebuddyUpstreams))
	if len(spec.AccountIDs) > 0 {
		for _, id := range normalizeStringList(spec.AccountIDs) {
			if upstream := m.codebuddyByID[id]; upstream != nil {
				selected = append(selected, upstream)
			}
		}
		return filterCodebuddyUpstreams(selected)
	}
	for i := range m.CodebuddyUpstreams {
		selected = append(selected, &m.CodebuddyUpstreams[i])
	}
	return filterCodebuddyUpstreams(selected)
}

func filterCodebuddyUpstreams(upstreams []*codebuddyUpstreamSpec) []*codebuddyUpstreamSpec {
	out := make([]*codebuddyUpstreamSpec, 0, len(upstreams))
	for _, upstream := range upstreams {
		if upstream == nil || upstream.Disabled {
			continue
		}
		if strings.TrimSpace(upstream.AccessToken) == "" || strings.TrimSpace(upstream.BaseURL) == "" {
			continue
		}
		out = append(out, upstream)
	}
	return out
}

// codebuddyCatalogForAPIKey merges the model list of every scoped upstream.
// Disabled operator models are omitted (aligned with chat rejection).
func codebuddyCatalogForAPIKey(m *manifest, spec *apiKeySpec) []string {
	upstreams := codebuddyUpstreamsForAPIKey(m, spec)
	night := codebuddyNightFreeFromManifest(m)
	now := time.Now()
	models := make([]string, 0)
	seen := make(map[string]struct{})
	anyAccountCatalog := false
	appendModel := func(model string) {
		key := strings.ToLower(strings.TrimSpace(model))
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		if codebuddyModelDisabled(m, model) {
			return
		}
		if ok, _ := night.AllowModel(model, now); !ok {
			return
		}
		seen[key] = struct{}{}
		models = append(models, model)
	}
	for _, upstream := range upstreams {
		if len(upstream.ModelIDs) == 0 {
			continue
		}
		anyAccountCatalog = true
		for _, model := range upstream.ModelIDs {
			appendModel(model)
		}
	}
	if !anyAccountCatalog {
		for _, id := range codebuddyDefaultModelIDs {
			appendModel(id)
		}
	}
	if spec != nil {
		if prefix := strings.Trim(strings.TrimSpace(spec.ModelPrefix), "/"); prefix != "" {
			prefixed := make([]string, 0, len(models))
			for _, model := range models {
				prefixed = append(prefixed, prefix+"/"+model)
			}
			return prefixed
		}
	}
	return models
}

func codebuddyUpstreamSupportsModel(upstream *codebuddyUpstreamSpec, model string) bool {
	if upstream == nil || len(upstream.ModelIDs) == 0 {
		return true
	}
	for _, candidate := range upstream.ModelIDs {
		if strings.EqualFold(candidate, model) {
			return true
		}
	}
	return false
}

// handleCodebuddyModels answers GET /v1/models for a CodeBuddy key.
func (s *relayServer) handleCodebuddyModels(c *gin.Context, spec *apiKeySpec) {
	ids := codebuddyCatalogForAPIKey(s.manifest, spec)
	if s.manifest != nil && len(s.manifest.DisabledModels) > 0 {
		filtered := ids[:0]
		for _, id := range ids {
			if !codebuddyModelDisabled(s.manifest, id) {
				filtered = append(filtered, id)
			}
		}
		ids = filtered
	}
	c.JSON(http.StatusOK, codebuddyModelsResponse(ids))
}

func codebuddyLooksClientAborted(message string) bool {
	lowerMsg := strings.ToLower(message)
	return strings.Contains(lowerMsg, "context canceled") ||
		strings.Contains(lowerMsg, "context cancelled") ||
		strings.Contains(lowerMsg, "client closed")
}

// handleCodebuddySessionsClear drops sticky session bindings.
func (s *relayServer) handleCodebuddySessionsClear(c *gin.Context) {
	cleared := codebuddyAffinity.ClearAll()
	c.JSON(http.StatusOK, gin.H{
		"cleared": cleared,
		"message": fmt.Sprintf("cleared %d sticky session binding(s)", cleared),
	})
}

func (s *relayServer) handleCodebuddyStatus(c *gin.Context) {
	gov := codebuddyGovernanceInit()
	pool := codebuddyPoolInit()
	if s.manifest != nil {
		pool.SyncFromManifest(s.manifest.CodebuddyUpstreams)
	}
	accounts := gov.statusLedger()
	summary := gin.H{
		"promptMode": codebuddyPromptMode(),
		"accounts":   accounts,
	}
	// Flatten rate_limited_models for convenience (issue #36 style).
	var rateLimited []map[string]any
	for _, acc := range accounts {
		if models, ok := acc["rateLimitedModels"].([]map[string]any); ok {
			for _, m := range models {
				row := map[string]any{"id": acc["id"]}
				for k, v := range m {
					row[k] = v
				}
				rateLimited = append(rateLimited, row)
			}
		}
	}
	if rateLimited == nil {
		rateLimited = []map[string]any{}
	}
	summary["rateLimitedModels"] = rateLimited
	// Cooling rows for UI: account + model + until.
	type coolRow struct {
		AccountID    string `json:"accountId"`
		AccountLabel string `json:"accountLabel"`
		Model        string `json:"model"`
		Kind         string `json:"kind"`
		Until        string `json:"until"`
		Reason       string `json:"reason,omitempty"`
	}
	var cooling []coolRow
	for _, acc := range accounts {
		if models, ok := acc["rateLimitedModels"].([]map[string]any); ok {
			for _, m := range models {
				modelName, _ := m["model"].(string)
				until, _ := m["until"].(string)
				cooling = append(cooling, coolRow{
					AccountID: fmt.Sprint(acc["id"]), AccountLabel: fmt.Sprint(acc["label"]),
					Model: modelName, Kind: "model", Until: until, Reason: fmt.Sprint(m["reason"]),
				})
			}
		}
		if until, ok := acc["until"].(string); ok && until != "" {
			cooling = append(cooling, coolRow{
				AccountID: fmt.Sprint(acc["id"]), AccountLabel: fmt.Sprint(acc["label"]),
				Model: "*", Kind: fmt.Sprint(acc["coolKind"]), Until: until, Reason: fmt.Sprint(acc["reason"]),
			})
		}
	}
	if cooling == nil {
		cooling = []coolRow{}
	}
	summary["cooling"] = cooling
	summary["sessionAffinity"] = codebuddyAffinity.Snapshot()
	// Keep the status payload small: ledger tab uses /requests for paging.
	summary["recentRequests"] = codebuddyRequestLogInit().List(20)
	summary["requestStats"] = codebuddyRequestLogInit().StatsByAccountModel()
	summary["modelUsage"] = codebuddyRequestLogInit().ModelUsageStats()
	summary["sessionAffinityCount"] = len(codebuddyAffinity.Snapshot())
	summary["apiKeyCooling"] = codebuddyKeys.status()
	summary["pool"] = pool.Ledger()
	if s.manifest != nil {
		summary["modelCredits"] = s.manifest.ModelCredits
		summary["modelEfforts"] = s.manifest.ModelEfforts
		summary["disabledModels"] = s.manifest.DisabledModels
		setCodebuddyModelEfforts(s.manifest.ModelEfforts)
	}
	summary["costExplore"] = CostExploreConfig()
	summary["nightFree"] = codebuddyNightFreeFromManifest(s.manifest).Status(time.Now())
	summary["catalog"] = BuildCatalog(s.manifest)
	summary["apiKeyStats"] = codebuddyRequestLogInit().StatsByAPIKey()
	if s.manifest != nil {
		summary["modelGroups"] = s.manifest.ModelGroups
		summary["accountModelCatalogs"] = s.manifest.AccountModelCatalogs
		type accCat struct {
			AccountID string            `json:"accountId"`
			Label     string            `json:"label"`
			Models    []map[string]any  `json:"models"`
			Credits   map[string]string `json:"credits,omitempty"`
			AlwaysFree []string         `json:"alwaysFreeModels,omitempty"`
			NightFree  []string         `json:"nightOnlyFreeModels,omitempty"`
		}
		var accCats []accCat
		for _, u := range s.manifest.CodebuddyUpstreams {
			credits := map[string]string{}
			for _, row := range u.ModelCatalog {
				if id, _ := row["id"].(string); id != "" {
					if c, _ := row["credits"].(string); c != "" {
						credits[id] = c
					}
				}
			}
			accCats = append(accCats, accCat{
				AccountID:  u.ID,
				Label:      u.Label,
				Models:     u.ModelCatalog,
				Credits:    credits,
				AlwaysFree: u.AlwaysFreeModels,
				NightFree:  u.NightOnlyFreeModels,
			})
		}
		if len(accCats) > 0 {
			summary["accountCatalogs"] = accCats
		}
	}
	c.JSON(http.StatusOK, summary)
}

// handleCodebuddyCatalog returns the merged model catalog (params + free/paid + usage).
func (s *relayServer) handleCodebuddyCatalog(c *gin.Context) {
	rows := BuildCatalog(s.manifest)
	night := codebuddyNightFreeFromManifest(s.manifest)
	now := time.Now()
	// Annotate night-free models.
	for i := range rows {
		if night.IsNightFreeModel(rows[i].ID) {
			in := night.InNightWindow(now)
			rows[i].Tags = append(rows[i].Tags, "night-free")
			if !in {
				rows[i].Disabled = true
				rows[i].Description = strings.TrimSpace(rows[i].Description + " [窗外禁用，防计费]")
			} else {
				free := true
				rows[i].FreeObserved = &free
			}
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"models":       rows,
		"disabled":     s.manifest.DisabledModels,
		"costExplore":  CostExploreConfig(),
		"nightFree":    night.Status(now),
		"modelCredits": s.manifest.ModelCredits,
	})
}

func codebuddyModelDisabled(m *manifest, model string) bool {
	if m == nil {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, id := range m.DisabledModels {
		if strings.ToLower(strings.TrimSpace(id)) == lower {
			return true
		}
	}
	return false
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// handleCodebuddyReturns returns a paginated request ledger (most recent first).
func (s *relayServer) handleCodebuddyRequests(c *gin.Context) {
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		if n := parseOptionalInt(raw); n > 0 {
			limit = n
		}
	}
	if limit > codebuddyRequestLogMax {
		limit = codebuddyRequestLogMax
	}
	offset := 0
	if raw := c.Query("offset"); raw != "" {
		if n := parseOptionalInt(raw); n > 0 {
			offset = n
		}
	}
	if offset > codebuddyRequestLogMax {
		offset = codebuddyRequestLogMax
	}
	store := codebuddyRequestLogInit()
	apiKeyFilter := strings.TrimSpace(c.Query("apiKey"))
	records, total := store.ListPage(offset, limit, apiKeyFilter)
	c.JSON(http.StatusOK, gin.H{
		"total":      total,
		"offset":     offset,
		"limit":      limit,
		"records":    records,
		"stats":      store.StatsByAccountModel(),
		"modelUsage": store.ModelUsageStats(),
		"apiKeyStats": store.StatsByAPIKey(),
		"apiKey":     apiKeyFilter,
	})
}

func codebuddyModelsResponse(models []string) gin.H {
	data := make([]gin.H, 0, len(models))
	for _, model := range models {
		data = append(data, gin.H{
			"id":       model,
			"object":   "model",
			"created":  0,
			"owned_by": "codebuddy",
		})
	}
	return gin.H{"object": "list", "data": data}
}

// handleCodebuddyChat proxies POST /v1/chat/completions to the CodeBuddy
// gateway. It may retry another account as long as nothing has been written to
// the client yet.
func (s *relayServer) handleCodebuddyChat(c *gin.Context, spec *apiKeySpec, clientBody []byte) {
	gov := codebuddyGovernanceInit()
	keyID := ""
	if spec != nil {
		keyID = spec.ID
		if keyID == "" {
			keyID = spec.Label
		}
	}
	if blocked, reason := codebuddyKeys.blocked(keyID); blocked {
		writeAPIError(c, http.StatusTooManyRequests, reason, "api_key_cooling")
		return
	}
	upstreams := codebuddyUpstreamsForAPIKey(s.manifest, spec)
	if len(upstreams) == 0 {
		writeAPIError(c, http.StatusServiceUnavailable,
			"no enabled CodeBuddy upstream account is configured", "no_upstream_account")
		return
	}

	model := stripModelPrefix(requestBodyModel(clientBody), spec)
	if model == "" {
		writeAPIError(c, http.StatusBadRequest, "model is required", "invalid_request")
		return
	}
	// Realm prefix routing: cn:/global: selects matching upstreams.
	realmPref, bareModel := codebuddySplitRealmModel(model)
	if bareModel != "" {
		model = bareModel
	}
	if realmPref != "" {
		filtered := upstreams[:0]
		for _, u := range upstreams {
			r := strings.ToLower(u.Realm)
			if r == "" {
				r = codebuddyInferRealm(u)
			}
			if r == realmPref {
				filtered = append(filtered, u)
			}
		}
		if len(filtered) > 0 {
			upstreams = filtered
		}
	}

	catalog := codebuddyCatalogForAPIKey(s.manifest, spec)
	if len(catalog) > 0 && !stringSliceContainsFold(catalog, model) {
		writeAPIError(c, http.StatusNotFound, fmt.Sprintf("model %s not found", model), "model_not_found")
		return
	}
	if codebuddyModelDisabled(s.manifest, model) {
		writeAPIError(c, http.StatusNotFound,
			fmt.Sprintf("model %s is disabled by operator", model), "model_disabled")
		return
	}
	night := codebuddyNightFreeFromManifest(s.manifest)
	if ok, reason := night.AllowModel(model, time.Now()); !ok {
		writeAPIError(c, http.StatusForbidden, reason, "model_outside_free_window")
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(clientBody, &payload); err != nil {
		writeAPIError(c, http.StatusBadRequest, "invalid request body", "invalid_request")
		return
	}

	// Conversation aggregation key (#170 turn-level, aligned with official CLI):
	// client X-Conversation-Request-ID wins; otherwise turn-level key when a user
	// turn exists (same turn retries share the key; next user message rotates);
	// session-stable fallback when no turn signature; else request-random.
	conversationRequestID := strings.TrimSpace(c.Request.Header.Get("X-Conversation-Request-ID"))
	sessionKey := codebuddyConversationKeyFrom(c.Request.Header, payload)
	// Sticky key = session key (conv/pck/hist); already suppresses user_id fallbacks.
	stickyKey := sessionKey
	turnSig := codebuddyTurnSignature(payload)
	if conversationRequestID == "" {
		if turnSig != "" {
			conversationRequestID = deriveTurnRequestID(sessionKey, turnSig)
		} else if sessionKey != "" {
			conversationRequestID = deriveStableRequestID(sessionKey)
		} else {
			conversationRequestID = randomHex32()
		}
	}

	promptMode := codebuddyPromptMode()
	clientWantsStream := requestBodyStream(clientBody)
	// firstBlocked tracks passthrough fingerprint false-positive degraded retry.
	degradedApplied := false
	tried := 0
	maxTokens, temperature, topP, msgCount, systemChars, toolCount, toolChoice := extractRequestMeta(payload)

	attachMeta := func(rec codebuddyRequestRecord, upstream *codebuddyUpstreamSpec) codebuddyRequestRecord {
		rec.MaxTokens = maxTokens
		rec.Temperature = temperature
		rec.TopP = topP
		rec.MessageCount = msgCount
		rec.SystemChars = systemChars
		rec.ToolCount = toolCount
		rec.ToolChoice = toolChoice
		rec.PromptMode = promptMode
		rec.IncludeReasoning = requestedReasoning(upstream)
		rec.UpstreamStream = true
		rec.Attempt = tried + 1
		rec.DegradedPrompt = degradedApplied
		rec.APIKeyID = keyID
		if spec != nil {
			rec.APIKeyLabel = spec.Label
			if rec.APIKeyLabel == "" {
				rec.APIKeyLabel = spec.ID
			}
		}
		return rec
	}

	order := codebuddySelectionOrder(s.manifest, upstreams, model)
	var stickyAccountID string
	order, stickyAccountID = pickUpstreamForSession(order, stickyKey, gov, model)
	pool := codebuddyPoolInit()
	if stickyAccountID != "" {
		s.emitExecutorDiagnostic(c, "codebuddy_session_sticky", model, stickyAccountID, time.Now(),
			"reuse conversation-bound account for prompt cache")
	}
	if pool.WafIPActive() {
		writeAPIError(c, http.StatusServiceUnavailable,
			"upstream WAF blocked this egress IP; retry after cool-down", "waf_ip_blocked")
		return
	}

	// Sticky session: if the bound account is cooling, UNBIND and rotate to the
	// next healthy account (do not pin the client to a cooling account, and do
	// not 503 when other accounts can serve). Only when every candidate is
	// cooling do we refuse without calling upstream.
	if stickyAccountID != "" {
		if ok, reason := gov.available(stickyAccountID, model); !ok {
			codebuddyAffinity.Unbind(stickyKey)
			s.emitExecutorDiagnostic(c, "codebuddy_sticky_unbind", model, stickyAccountID, time.Now(), reason)
		} else if ok, reason := pool.Available(stickyAccountID, model); !ok {
			codebuddyAffinity.Unbind(stickyKey)
			s.emitExecutorDiagnostic(c, "codebuddy_sticky_unbind", model, stickyAccountID, time.Now(), reason)
		}
	}

	// Pre-count usable candidates; if none, do not touch upstream at all.
	usable := 0
	for _, u := range order {
		if u == nil || !codebuddyUpstreamSupportsModel(u, model) {
			continue
		}
		if ok, _ := gov.available(u.ID, model); !ok {
			continue
		}
		if ok, _ := pool.Available(u.ID, model); !ok {
			continue
		}
		usable++
	}
	if usable == 0 {
		writeAPIError(c, http.StatusServiceUnavailable,
			"all accounts cooling for this model; not calling upstream", "all_accounts_cooling")
		return
	}

	var lastStatus int
	var lastMessage string
	rotateAttempt := 0
	hitRateLimit := false
	for _, upstream := range order {
		if !codebuddyUpstreamSupportsModel(upstream, model) {
			continue
		}
		attemptStarted := time.Now()
		if ok, reason := gov.available(upstream.ID, model); !ok {
			s.emitExecutorDiagnostic(c, "codebuddy_account_cooling", model, upstream.ID, attemptStarted, reason)
			codebuddyRecordRequest(attachMeta(codebuddyRequestRecord{
				AccountID:             upstream.ID,
				AccountLabel:          upstream.Label,
				Model:                 model,
				ClientStream:          clientWantsStream,
				Outcome:               "cooling",
				Message:               reason,
				ReasonCode:            "cooling",
				LatencyMs:             time.Since(attemptStarted).Milliseconds(),
				ConversationRequestID: conversationRequestID,
			}, upstream))
			if stickyKey != "" {
				codebuddyAffinity.Unbind(stickyKey)
			}
			continue
		}
		if ok, reason := pool.Available(upstream.ID, model); !ok {
			s.emitExecutorDiagnostic(c, "codebuddy_pool_skip", model, upstream.ID, attemptStarted, reason)
			codebuddyRecordRequest(attachMeta(codebuddyRequestRecord{
				AccountID: upstream.ID, AccountLabel: upstream.Label, Model: model,
				ClientStream: clientWantsStream, Outcome: "cooling",
				ReasonCode: "pool", Message: reason,
				LatencyMs: time.Since(attemptStarted).Milliseconds(),
				ConversationRequestID: conversationRequestID,
			}, upstream))
			if stickyKey != "" {
				codebuddyAffinity.Unbind(stickyKey)
			}
			continue
		}
		// After a rate-limit on this request, do not keep burning the pool.
		if hitRateLimit && rotateAttempt >= 1 {
			break
		}

		bodyForAttempt := payload
		// Prompt policy is applied once inside encode (degraded=degradedApplied).
		// Pre-applying here then re-applying in encode would double-insert append.
		upstreamBody, err := codebuddyEncodeRequestBody(bodyForAttempt, model, upstream, c, conversationRequestID, degradedApplied)
		if err != nil {
			writeAPIError(c, http.StatusBadRequest, "failed to encode request body", "invalid_request")
			return
		}

		pool.Acquire(upstream.ID)
		status, message, rawBody, resp := s.openCodebuddyStream(c, upstream, upstreamBody, model)
		pool.Release(upstream.ID)
		latencyMs := time.Since(attemptStarted).Milliseconds()
		if resp != nil {

			defer resp.Body.Close()
			gov.noteSuccess(upstream.ID)
			pool.NoteSuccess(upstream.ID)
			codebuddyKeys.noteOK(keyID)
			if stickyKey != "" {
				codebuddyAffinity.Bind(stickyKey, upstream.ID)
			}
			rec := attachMeta(codebuddyRequestRecord{
				AccountID:             upstream.ID,
				AccountLabel:          upstream.Label,
				Model:                 model,
				ClientStream:          clientWantsStream,
				Outcome:               "ok",
				HTTPStatus:            http.StatusOK,
				LatencyMs:             latencyMs,
				ConversationRequestID: conversationRequestID,
			}, upstream)
			usage, firstTokenMs := s.writeCodebuddyResponse(c, resp, model, clientWantsStream, requestedReasoning(upstream), attemptStarted)
			rec.FirstTokenMs = firstTokenMs
			rec.TotalMs = time.Since(attemptStarted).Milliseconds()
			if usage != nil {
				rec.PromptTokens = usage.PromptTokens
				rec.CompletionTokens = usage.CompletionTokens
				rec.TotalTokens = usage.TotalTokens
				if usage.HasCredit {
					rec.Credit = usage.Credit
					rec.HasCredit = true
					pool.NoteModelCost(upstream.ID, model, usage.Credit, int(usage.TotalTokens))
				}
				rec.CachedTokens = usage.CachedTokens
				rec.CacheWriteTokens = usage.CacheWriteTokens
				rec.ReasoningTokens = usage.ReasoningTokens
			}
			codebuddyRecordRequest(rec)
			return
		}
		// Client aborted / timed out: do NOT cool the account or rotate the pool.
		// (用户中断/超时不是账号故障，记冷却会误伤并导致后续一直 503。)
		if codebuddyLooksClientAborted(message) || c.Request.Context().Err() != nil {
			codebuddyKeys.noteOK(keyID) // abort is not a key fault either
			if stickyKey != "" {
				// keep sticky binding — client may retry the same conversation
				_ = stickyAccountID
			}
			codebuddyRecordRequest(attachMeta(codebuddyRequestRecord{
				AccountID:             upstream.ID,
				AccountLabel:          upstream.Label,
				Model:                 model,
				ClientStream:          clientWantsStream,
				Outcome:               "client_aborted",
				ReasonCode:            "client_aborted",
				Message:               truncateForLog(message, 200),
				LatencyMs:             latencyMs,
				ConversationRequestID: conversationRequestID,
			}, upstream))
			if c.Request.Context().Err() != nil {
				return
			}
			writeAPIError(c, 499, "client aborted request (account not cooled)", "client_aborted")
			return
		}
		tried++
		rotateAttempt++
		// Rotate backoff (workbuddy2api P0-2): small exponential pause between accounts.
		if rotateAttempt > 1 {
			shift := rotateAttempt - 2
			if shift > 4 {
				shift = 4
			}
			d := time.Duration(50*(1<<shift)) * time.Millisecond
			jitter := time.Duration(rand.Int63n(int64(d)/2 + 1))
			select {
			case <-c.Request.Context().Done():
				return
			case <-time.After(d/2 + jitter):
			}
		}
		lastStatus, lastMessage = status, message
		outcome, reasonCode := classifyCodebuddyOutcome(status, rawBody)
		resetAt := parseRateResetAt(rawBody)
		errDetail := truncateForLog(rawBody, 240)
		if errDetail == "" {
			errDetail = truncateForLog(message, 240)
		}

		// WAF 403: account soft-cooldown + possible IP fail-fast.
		if codebuddyLooksWafBlocked(status, rawBody) || codebuddyLooksWafBlocked(status, message) {
			ipBlocked := pool.NoteWaf(upstream.ID)
			codebuddyRecordRequest(attachMeta(codebuddyRequestRecord{
				AccountID: upstream.ID, AccountLabel: upstream.Label, Model: model,
				ClientStream: clientWantsStream, Outcome: "waf",
				HTTPStatus: status, LatencyMs: latencyMs,
				Message: errDetail, ReasonCode: "waf_403",
				ConversationRequestID: conversationRequestID,
			}, upstream))
			if sessionKey != "" || stickyKey != "" {
				codebuddyAffinity.Unbind(stickyKey)
			}
			if ipBlocked {
				writeAPIError(c, http.StatusServiceUnavailable,
					"upstream WAF blocked this egress IP; retry after cool-down", "waf_ip_blocked")
				return
			}
			continue
		}

		// Apply governance / degraded retry based on failure shape.
		if rawBody != "" {
			if isModelRateLimit(rawBody) {
				// Model-level only: other models stay selectable immediately.
				hitRateLimit = true
				gov.coolSoftModel(upstream.ID, model, resetAt, "6004 model rate limit")
				if stickyKey != "" {
					codebuddyAffinity.Unbind(stickyKey)
				}
				codebuddyRecordRequest(attachMeta(codebuddyRequestRecord{
					AccountID:             upstream.ID,
					AccountLabel:          upstream.Label,
					Model:                 model,
					ClientStream:          clientWantsStream,
					Outcome:               outcome,
					HTTPStatus:            status,
					LatencyMs:             latencyMs,
					Message:               errDetail,
					ReasonCode:            reasonCode,
					ResetAt:               formatOptionalTime(resetAt),
					ConversationRequestID: conversationRequestID,
				}, upstream))
				continue
			}
			if codebuddyIsHardQuota(rawBody, status) {
				gov.coolHardQuota(upstream.ID, "balance exhausted")
				codebuddyRecordRequest(attachMeta(codebuddyRequestRecord{
					AccountID: upstream.ID, AccountLabel: upstream.Label, Model: model,
					ClientStream: clientWantsStream, Outcome: outcome, HTTPStatus: status,
					LatencyMs: latencyMs, Message: errDetail, ReasonCode: reasonCode,
					ConversationRequestID: conversationRequestID,
				}, upstream))
				continue
			}
			if codebuddyLooksContentBlocked(status, rawBody) {
				codebuddyRecordRequest(attachMeta(codebuddyRequestRecord{
					AccountID: upstream.ID, AccountLabel: upstream.Label, Model: model,
					ClientStream: clientWantsStream, Outcome: outcome, HTTPStatus: status,
					LatencyMs: latencyMs, Message: errDetail, ReasonCode: reasonCode,
					ConversationRequestID: conversationRequestID,
				}, upstream))
				// 内容拦截误报（passthrough/append 首遇）：降级到中性提示词重试；
				// append 降级时 applySystemPromptMode 会退化为 replace。
				if (promptMode == "passthrough" || promptMode == "append") && !degradedApplied {
					degradedApplied = true
					s.emitExecutorDiagnostic(c, "codebuddy_content_blocked_degrade", model, upstream.ID, time.Now(), "retry with neutral system")
					continue
				}
				writeAPIError(c, http.StatusBadRequest, "content blocked by upstream moderation", "content_blocked")
				return
			}
			if status == http.StatusTooManyRequests {
				hitRateLimit = true
				gov.coolSoftRate(upstream.ID, resetAt, "429 soft rate")
				if stickyKey != "" {
					codebuddyAffinity.Unbind(stickyKey)
				}
				codebuddyRecordRequest(attachMeta(codebuddyRequestRecord{
					AccountID: upstream.ID, AccountLabel: upstream.Label, Model: model,
					ClientStream: clientWantsStream, Outcome: outcome, HTTPStatus: status,
					LatencyMs: latencyMs, Message: errDetail, ReasonCode: reasonCode,
					ResetAt: formatOptionalTime(resetAt), ConversationRequestID: conversationRequestID,
				}, upstream))
				continue
			}
			if status == http.StatusNotFound {
				gov.coolShallow(upstream.ID, "404 shallow")
			}
		}
		if status == http.StatusUnauthorized {
			gov.markAuthFailed(upstream.ID, "credential rejected")
			codebuddyRecordRequest(codebuddyRequestRecord{
				AccountID: upstream.ID, AccountLabel: upstream.Label, Model: model,
				ClientStream: clientWantsStream, Outcome: outcome, HTTPStatus: status,
				LatencyMs: latencyMs, Message: errDetail, ReasonCode: reasonCode,
				ConversationRequestID: conversationRequestID,
			})
			continue
		}
		if status >= 500 || status == http.StatusBadGateway {
			gov.noteFailure(upstream.ID, "upstream failure")
			pool.NoteFailure(upstream.ID, "upstream failure")
		}
		if strings.Contains(rawBody, "12153") || strings.Contains(message, "12153") {
			pool.NoteSessionDead(upstream.ID)
		}
		codebuddyRecordRequest(codebuddyRequestRecord{
			AccountID: upstream.ID, AccountLabel: upstream.Label, Model: model,
			ClientStream: clientWantsStream, Outcome: outcome, HTTPStatus: status,
			LatencyMs: latencyMs, Message: errDetail, ReasonCode: reasonCode,
			ConversationRequestID: conversationRequestID,
		})
	}

	if lastStatus == 0 {
		if tried == 0 {
			writeAPIError(c, http.StatusServiceUnavailable,
				"all CodeBuddy accounts are cooling or unavailable", "all_accounts_unavailable")
			return
		}
		lastStatus = http.StatusBadGateway
	}
	if strings.TrimSpace(lastMessage) == "" {
		lastMessage = "all CodeBuddy upstream accounts failed"
	}
	writeAPIError(c, codebuddyHTTPStatus(lastStatus), lastMessage, "upstream_error")
}

func codebuddyPromptMode() string {
	mode := strings.TrimSpace(os.Getenv("COCKPIT_CODEBUDDY_PROMPT_MODE"))
	if strings.EqualFold(mode, "custom") {
		return "custom"
	}
	// append：开头连续 system/developer 块后插网关 system，既有消息逐字不动
	//（对齐 workbuddy2api prompt.mode=append，issue #129）。
	if strings.EqualFold(mode, "append") {
		return "append"
	}
	return "passthrough"
}

// codebuddyEncodeRequestBody builds the outbound body: stream:true, thinking
// injection, reasoning backfill, system prompt policy, fingerprint sanitize.
// degraded=true applies the content-block rescue rewrite (append degrades to replace).
func codebuddyEncodeRequestBody(
	payload map[string]any,
	model string,
	upstream *codebuddyUpstreamSpec,
	c *gin.Context,
	conversationRequestID string,
	degraded bool,
) ([]byte, error) {
	cloned := make(map[string]any, len(payload)+8)
	for k, v := range payload {
		cloned[k] = v
	}
	cloned["model"] = model
	cloned["stream"] = true
	delete(cloned, "stream_options")
	delete(cloned, "logprobs")
	delete(cloned, "top_logprobs")
	if value, ok := cloned["n"].(float64); ok && value > 1 {
		delete(cloned, "n")
	}
	if _, ok := cloned["max_tokens"]; !ok {
		if value, ok := cloned["max_completion_tokens"]; ok {
			cloned["max_tokens"] = value
		}
	}
	delete(cloned, "max_completion_tokens")
	// Official CLI always requests usage on the last SSE frame (credit + tokens).
	if _, exists := cloned["stream_options"]; !exists {
		cloned["stream_options"] = map[string]any{"include_usage": true}
	}

	applySystemPromptMode(cloned, codebuddyPromptMode(), degraded)
	injectDeepSeekThinking(cloned, model)
	backfillReasoningContent(cloned)
	sanitizeRequestPayload(cloned)
	return json.Marshal(cloned)
}

// codebuddySelectionOrder applies pool intelligence first, then falls back to
// the legacy RR/random rotation when the pool has no differentiating state.
func codebuddySelectionOrder(m *manifest, upstreams []*codebuddyUpstreamSpec, model string) []*codebuddyUpstreamSpec {
	if len(upstreams) <= 1 {
		return upstreams
	}
	pool := codebuddyPoolInit()
	pool.SyncFromPointers(upstreams)
	routing := ""
	if m != nil {
		routing = m.RoutingStrategy
	}
	ordered := pool.PickOrder(upstreams, model, "", routing)
	if len(ordered) == len(upstreams) {
		return ordered
	}
	// Defensive: pool must return the same multiset.
	return ordered
}

func codebuddySplitRealmModel(model string) (realm, bare string) {
	model = strings.TrimSpace(model)
	if i := strings.Index(model, ":"); i > 0 {
		pref := strings.ToLower(model[:i])
		if pref == "cn" || pref == "global" || pref == "intl" {
			if pref == "intl" {
				pref = "global"
			}
			return pref, model[i+1:]
		}
	}
	return "", model
}

func codebuddyLooksWafBlocked(status int, body string) bool {
	if status != http.StatusForbidden {
		return false
	}
	lower := strings.ToLower(body)
	// Business envelope → not WAF.
	if strings.Contains(body, `"code"`) && strings.Contains(lower, `"msg"`) {
		return false
	}
	if strings.TrimSpace(body) == "" {
		return true
	}
	// HTML / APISIX / non-envelope payloads.
	if strings.Contains(lower, "<html") || strings.Contains(lower, "apisix") ||
		strings.Contains(lower, "blocked") || strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "access denied") {
		return true
	}
	// Pure text without JSON envelope.
	if !strings.Contains(lower, "{") {
		return true
	}
	return false
}

func requestedReasoning(upstream *codebuddyUpstreamSpec) bool {
	return upstream != nil && upstream.IncludeReasoning
}

// openCodebuddyStream performs the upstream call. It returns (status, message,
// body, nil) when the upstream rejected the request, so the caller can apply
// governance and try the next account. A non-nil response is always HTTP 200
// with an open stream.
func (s *relayServer) openCodebuddyStream(c *gin.Context, upstream *codebuddyUpstreamSpec, body []byte, model string) (int, string, string, *http.Response) {
	startedAt := time.Now()
	resp, err := s.doCodebuddyRequest(c, upstream, upstream.AccessToken, body)
	if err != nil {
		s.emitExecutorDiagnostic(c, "codebuddy_request_failed", model, upstream.ID, startedAt, err.Error())
		return http.StatusBadGateway, err.Error(), "", nil
	}

	if resp.StatusCode == http.StatusUnauthorized && strings.TrimSpace(upstream.RefreshToken) != "" {
		_ = resp.Body.Close()
		refreshed, refreshErr := refreshCodebuddyToken(c, upstream)
		if refreshErr != nil {
			s.emitExecutorDiagnostic(c, "codebuddy_token_refresh_failed", model, upstream.ID, startedAt, refreshErr.Error())
			return http.StatusUnauthorized, "CodeBuddy credential expired and could not be refreshed", "", nil
		}
		resp, err = s.doCodebuddyRequest(c, upstream, refreshed, body)
		if err != nil {
			return http.StatusBadGateway, err.Error(), "", nil
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		raw := string(payload)
		status, message := codebuddyUpstreamErrorFromBody(raw, resp.StatusCode)
		s.emitExecutorDiagnostic(c, "codebuddy_upstream_error", model, upstream.ID, startedAt,
			fmt.Sprintf("status=%d message=%s", status, message))
		return status, message, raw, nil
	}
	return http.StatusOK, "", "", resp
}

func (s *relayServer) doCodebuddyRequest(c *gin.Context, upstream *codebuddyUpstreamSpec, token string, body []byte) (*http.Response, error) {
	url := strings.TrimRight(upstream.BaseURL, "/") + codebuddyChatCompletionsPath
	req, err := http.NewRequestWithContext(relayContext(c), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", codebuddyContentType)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", codebuddyUserAgent(upstream))
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Prefer branded origin when available via env; default to base host.
	req.Header.Set("Origin", codebuddyOriginFor(upstream))
	req.Header.Set("Referer", codebuddyOriginFor(upstream)+"/")
	req.Header.Set("Accept-Language", codebuddyAcceptLanguage(upstream))
	injectAccountStableHeaders(req, upstream.UID)

	var clientBody map[string]any
	_ = json.Unmarshal(body, &clientBody)
	applyConversationHeaders(req, c.Request, clientBody, "")

	intent := strings.TrimSpace(upstream.AgentIntent)
	if intent == "" {
		intent = codebuddyDefaultAgentIntent
	}
	req.Header.Set("X-Agent-Intent", intent)
	copyProviderGatewayDiagnosticHeaders(req.Header, c.Request.Header)
	return http.DefaultClient.Do(req)
}

func codebuddyOriginFor(upstream *codebuddyUpstreamSpec) string {
	base := strings.TrimRight(upstream.BaseURL, "/")
	lower := strings.ToLower(base)
	switch {
	case strings.Contains(lower, "workbuddy.ai"):
		return "https://www.workbuddy.ai"
	case strings.Contains(lower, "workbuddy.cn"):
		return "https://www.workbuddy.cn"
	case strings.Contains(lower, "copilot.tencent.com"):
		return "https://www.codebuddy.cn"
	default:
		return "https://www.codebuddy.cn"
	}
}

func codebuddyAcceptLanguage(upstream *codebuddyUpstreamSpec) string {
	lower := strings.ToLower(upstream.BaseURL)
	if strings.Contains(lower, "workbuddy.ai") {
		return "en-US"
	}
	return "zh-CN"
}

func codebuddyUserAgent(upstream *codebuddyUpstreamSpec) string {
	if upstream != nil {
		if ua := strings.TrimSpace(upstream.UserAgent); ua != "" {
			return ua
		}
	}
	return codebuddyDefaultUserAgent
}

// refreshCodebuddyToken performs the documented gateway refresh. The refreshed
// pair is reported to Cockpit through an event so Rust can persist it; the
// in-memory copy keeps this process working until the manifest is rewritten.
func refreshCodebuddyToken(c *gin.Context, upstream *codebuddyUpstreamSpec) (string, error) {
	url := strings.TrimRight(upstream.BaseURL, "/") + codebuddyTokenRefreshPath
	req, err := http.NewRequestWithContext(relayContext(c), http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+upstream.AccessToken)
	req.Header.Set("X-Refresh-Token", upstream.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codebuddyUserAgent(upstream))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("refresh rejected with status %d", resp.StatusCode)
	}
	var decoded struct {
		Code int `json:"code"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", err
	}
	if strings.TrimSpace(decoded.Data.AccessToken) == "" {
		return "", fmt.Errorf("refresh response did not contain an access token")
	}

	upstream.AccessToken = decoded.Data.AccessToken
	if strings.TrimSpace(decoded.Data.RefreshToken) != "" {
		upstream.RefreshToken = decoded.Data.RefreshToken
	}
	if emitter := globalCodebuddyEmitter; emitter != nil {
		emitter.emit(map[string]any{
			"type":         "codebuddy_token_refreshed",
			"upstreamId":   upstream.ID,
			"accessToken":  decoded.Data.AccessToken,
			"refreshToken": upstream.RefreshToken,
			"expiresIn":    decoded.Data.ExpiresIn,
		})
	}
	return upstream.AccessToken, nil
}

// codebuddyEventSink abstracts stdout event emission so tests can capture
// usage events without racing process stdout.
type codebuddyEventSink interface {
	emit(v any)
}

// globalCodebuddyEmitter mirrors the process emitter so a refresh that happens
// on a request path can still report back to Cockpit. main() assigns it.
var globalCodebuddyEmitter codebuddyEventSink

// codebuddyUpstreamError is retained for tests that still call the reader form.
func codebuddyUpstreamError(resp *http.Response) (int, string) {
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return codebuddyUpstreamErrorFromBody(string(payload), resp.StatusCode)
}

// codebuddyUpstreamErrorFromBody converts an upstream error body into a
// client-facing message plus the HTTP status the relay should surface.
func codebuddyUpstreamErrorFromBody(raw string, upstreamStatus int) (int, string) {
	message := strings.TrimSpace(raw)
	var decoded struct {
		Code       json.RawMessage `json:"code"`
		Msg        string          `json:"msg"`
		ErrorMsg   string          `json:"error_msg"`
		DisplayMsg struct {
			En string `json:"en"`
			Zh string `json:"zh"`
		} `json:"displayMsg"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
		switch {
		case strings.TrimSpace(decoded.DisplayMsg.Zh) != "":
			message = decoded.DisplayMsg.Zh
		case strings.TrimSpace(decoded.Msg) != "":
			message = decoded.Msg
		case strings.TrimSpace(decoded.DisplayMsg.En) != "":
			message = decoded.DisplayMsg.En
		case strings.TrimSpace(decoded.ErrorMsg) != "":
			message = decoded.ErrorMsg
		}
	}
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("CodeBuddy upstream returned status %d", upstreamStatus)
	}
	return codebuddyHTTPStatus(upstreamStatus), message
}

// codebuddyHTTPStatus keeps upstream status semantics but never exposes a 401
// from our own credential when the client key was already accepted.
func codebuddyHTTPStatus(status int) int {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return http.StatusBadGateway
	}
	if status < 400 || status > 599 {
		return http.StatusBadGateway
	}
	return status
}

// writeCodebuddyResponse streams (or aggregates) the upstream SSE body.
// Returns a usage snapshot when the last frame carried one, plus first-token latency.
func (s *relayServer) writeCodebuddyResponse(
	c *gin.Context,
	resp *http.Response,
	model string,
	clientWantsStream, includeReasoning bool,
	startedAt time.Time,
) (usage *codebuddyUsageSnapshot, firstTokenMs int64) {
	markFirst := func() {
		if firstTokenMs == 0 {
			firstTokenMs = time.Since(startedAt).Milliseconds()
		}
	}
	if !clientWantsStream {
		result, usage := codebuddyAggregateResponse(resp.Body, model, includeReasoning)
		markFirst()
		c.JSON(http.StatusOK, result)
		return usage, firstTokenMs
	}

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		writeAPIError(c, http.StatusInternalServerError, "streaming not supported", "streaming_not_supported")
		return nil, 0
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			_, _ = c.Writer.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return usage, firstTokenMs
		}
		chunk, err := decodeCodebuddyChunk(payload)
		if err != nil {
			continue
		}
		if message, isError := codebuddyInlineError(chunk); isError {
			_, _ = c.Writer.Write(codebuddyStreamErrorFrame(message))
			flusher.Flush()
			return usage, firstTokenMs
		}
		if snap := extractCodebuddyUsage(chunk); snap != nil {
			usage = snap
		}
		// First content-bearing frame → TTFT.
		if firstTokenMs == 0 {
			if choices, ok := chunk["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if delta, ok := choice["delta"].(map[string]any); ok {
						if text, _ := delta["content"].(string); text != "" {
							markFirst()
						}
					}
				}
			}
		}
		sanitizeCodebuddyChunk(chunk, model, includeReasoning)
		encoded, err := json.Marshal(chunk)
		if err != nil {
			continue
		}
		if _, err := c.Writer.Write([]byte("data: " + string(encoded) + "\n\n")); err != nil {
			return usage, firstTokenMs
		}
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		_, _ = c.Writer.Write(codebuddyStreamErrorFrame("CodeBuddy stream interrupted: " + err.Error()))
		flusher.Flush()
	}
	return usage, firstTokenMs
}

func decodeCodebuddyChunk(payload []byte) (map[string]any, error) {
	var chunk map[string]any
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, err
	}
	return chunk, nil
}

// codebuddyInlineError detects a gateway error delivered inside an SSE frame.
func codebuddyInlineError(chunk map[string]any) (string, bool) {
	if chunk == nil {
		return "", false
	}
	if _, hasChoices := chunk["choices"]; hasChoices {
		return "", false
	}
	message := strings.TrimSpace(stringValue(chunk["msg"]))
	if message == "" {
		message = strings.TrimSpace(stringValue(chunk["error_msg"]))
	}
	if message == "" {
		if display, ok := chunk["displayMsg"].(map[string]any); ok {
			message = strings.TrimSpace(stringValue(display["zh"]))
			if message == "" {
				message = strings.TrimSpace(stringValue(display["en"]))
			}
		}
	}
	if message == "" {
		return "", false
	}
	return message, true
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func codebuddyStreamErrorFrame(message string) []byte {
	payload, err := json.Marshal(gin.H{"error": gin.H{
		"message": message,
		"type":    "upstream_error",
		"code":    "upstream_error",
	}})
	if err != nil {
		return []byte("data: [DONE]\n\n")
	}
	return []byte("data: " + string(payload) + "\n\ndata: [DONE]\n\n")
}

// sanitizeCodebuddyChunk rewrites the model and removes gateway-specific fields
// that are not part of the OpenAI schema.
func sanitizeCodebuddyChunk(chunk map[string]any, model string, includeReasoning bool) {
	if chunk == nil {
		return
	}
	if strings.TrimSpace(model) != "" {
		chunk["model"] = model
	}
	choices, _ := chunk["choices"].([]any)
	for _, rawChoice := range choices {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			continue
		}
		delete(delta, "extra_fields")
		delete(delta, "logprobs")
		if refusal, ok := delta["refusal"].(string); ok && strings.TrimSpace(refusal) == "" {
			delete(delta, "refusal")
		}
		if functionCall, exists := delta["function_call"]; exists {
			if functionCall == nil {
				delete(delta, "function_call")
			} else if call, ok := functionCall.(map[string]any); ok &&
				strings.TrimSpace(stringValue(call["name"])) == "" &&
				strings.TrimSpace(stringValue(call["arguments"])) == "" {
				delete(delta, "function_call")
			}
		}
		reasoning, hasReasoning := delta["reasoning_content"].(string)
		switch {
		case !includeReasoning:
			delete(delta, "reasoning_content")
		case hasReasoning && reasoning == "":
			delete(delta, "reasoning_content")
		}
	}
	if usage, ok := chunk["usage"].(map[string]any); ok {
		for _, key := range []string{
			"credit", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens",
			"cache_read_input_tokens", "cache_creation_input_tokens",
			"prompt_cache_write_tokens", "completion_thinking_tokens", "cached_tokens",
		} {
			delete(usage, key)
		}
	}
}

// codebuddyUsageSnapshot is the gateway-facing usage/credit record.
type codebuddyUsageSnapshot struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	Credit           float64
	HasCredit        bool
	CachedTokens     int64
	CacheWriteTokens int64
	ReasoningTokens  int64
}

func extractCodebuddyUsage(chunk map[string]any) *codebuddyUsageSnapshot {
	if chunk == nil {
		return nil
	}
	raw, ok := chunk["usage"].(map[string]any)
	if !ok || raw == nil {
		return nil
	}
	snap := &codebuddyUsageSnapshot{}
	if v, ok := raw["prompt_tokens"].(float64); ok {
		snap.PromptTokens = int64(v)
	}
	if v, ok := raw["completion_tokens"].(float64); ok {
		snap.CompletionTokens = int64(v)
	}
	if v, ok := raw["total_tokens"].(float64); ok {
		snap.TotalTokens = int64(v)
	} else {
		snap.TotalTokens = snap.PromptTokens + snap.CompletionTokens
	}
	for _, key := range []string{"prompt_cache_hit_tokens", "cached_tokens", "cache_read_input_tokens"} {
		if v, ok := raw[key].(float64); ok && int64(v) > snap.CachedTokens {
			snap.CachedTokens = int64(v)
		}
	}
	for _, key := range []string{"prompt_cache_miss_tokens", "prompt_cache_write_tokens", "cache_creation_input_tokens"} {
		if v, ok := raw[key].(float64); ok && int64(v) > snap.CacheWriteTokens {
			snap.CacheWriteTokens = int64(v)
		}
	}
	if v, ok := raw["completion_tokens_details"].(map[string]any); ok {
		if r, ok := v["reasoning_tokens"].(float64); ok {
			snap.ReasoningTokens = int64(r)
		}
	}
	if v, ok := raw["reasoning_tokens"].(float64); ok && snap.ReasoningTokens == 0 {
		snap.ReasoningTokens = int64(v)
	}
	switch v := raw["credit"].(type) {
	case float64:
		snap.Credit = v
		snap.HasCredit = true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			snap.Credit = f
			snap.HasCredit = true
		}
	}
	return snap
}

func truncateForLog(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// codebuddyAggregateResponse folds the upstream stream into one
// `chat.completion` object for clients that requested `stream: false`.
//
// Ports workbuddy2api Aggregate fixes:
//   - empty content latch: only non-empty content sets gotAnyContent (issue #142)
//   - non-delta message fallback shares the latch (no double-append)
//   - tool_call missing index: id-priority / lastIdx / skip-assign (no merge pollution)
//   - usage missing total_tokens: synthesize prompt+completion when both present
//   - EOF-truncated tool_calls (no [DONE]) drop incomplete arguments
func codebuddyAggregateResponse(body io.Reader, model string, includeReasoning bool) (gin.H, *codebuddyUsageSnapshot) {
	var (
		id           string
		created      int64
		content      strings.Builder
		reasoning    strings.Builder
		finishReason = "stop"
		usage        map[string]any
		usageSnap    *codebuddyUsageSnapshot
		toolCalls    []map[string]any
		toolIndexes  = make(map[int]int)
		gotAnyContent bool
		sawDone       bool
		toolSeq       int
		idIndex       = make(map[string]int)
	)

	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
		gotAnyContent = true
	}

	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolIndexes[idx]; !used {
				return idx
			}
		}
	}

	appendToolCallDeltasFixed := func(raw any) {
		deltas, _ := raw.([]any)
		for _, rawDelta := range deltas {
			delta, ok := rawDelta.(map[string]any)
			if !ok {
				continue
			}
			index := -1
			if value, ok := delta["index"].(float64); ok {
				index = int(value)
			} else if cid, _ := delta["id"].(string); cid != "" {
				if mid, seen := idIndex[cid]; seen {
					index = mid
				} else {
					index = nextToolIndex()
				}
			} else if len(toolCalls) > 0 {
				index = len(toolCalls) - 1
			} else {
				index = nextToolIndex()
			}
			position, exists := toolIndexes[index]
			if !exists {
				position = len(toolCalls)
				toolIndexes[index] = position
				toolCalls = append(toolCalls, map[string]any{
					"index": index,
					"type":  "function",
					"function": map[string]any{
						"name":      "",
						"arguments": "",
					},
				})
			}
			if cid, _ := delta["id"].(string); cid != "" {
				idIndex[cid] = index
			}
			entry := toolCalls[position]
			if id := strings.TrimSpace(stringValue(delta["id"])); id != "" {
				entry["id"] = id
				idIndex[id] = index
			}
			if kind := strings.TrimSpace(stringValue(delta["type"])); kind != "" {
				entry["type"] = kind
			}
			function, _ := delta["function"].(map[string]any)
			if function == nil {
				continue
			}
			existing, _ := entry["function"].(map[string]any)
			if existing == nil {
				existing = map[string]any{"name": "", "arguments": ""}
				entry["function"] = existing
			}
			if name := stringValue(function["name"]); name != "" {
				// Name fragments arrive split across frames for the same call index
				// ("get_" + "weather"); concatenate. Do not replace.
				existing["name"] = stringValue(existing["name"]) + name
			}
			if arguments := stringValue(function["arguments"]); arguments != "" {
				existing["arguments"] = stringValue(existing["arguments"]) + arguments
			}
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			sawDone = true
			continue
		}
		chunk, err := decodeCodebuddyChunk(payload)
		if err != nil {
			continue
		}
		if message, isError := codebuddyInlineError(chunk); isError {
			return gin.H{"error": gin.H{
				"message": message,
				"type":    "upstream_error",
				"code":    "upstream_error",
			}}, usageSnap
		}
		if value := strings.TrimSpace(stringValue(chunk["id"])); value != "" {
			id = value
		}
		if value, ok := chunk["created"].(float64); ok && value > 0 {
			created = int64(value)
		}
		if value, ok := chunk["usage"].(map[string]any); ok && value != nil {
			usage = value
			usageSnap = extractCodebuddyUsage(chunk)
		}
		choices, _ := chunk["choices"].([]any)
		for _, rawChoice := range choices {
			choice, ok := rawChoice.(map[string]any)
			if !ok {
				continue
			}
			if reason := strings.TrimSpace(stringValue(choice["finish_reason"])); reason != "" {
				finishReason = reason
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				appendContent(stringValue(delta["content"]))
				if includeReasoning {
					reasoning.WriteString(stringValue(delta["reasoning_content"]))
				}
				appendToolCallDeltasFixed(delta["tool_calls"])
			}
			// Non-delta message fallback (some upstreams): merge once behind the latch.
			if msg, ok := choice["message"].(map[string]any); ok && !gotAnyContent {
				appendContent(stringValue(msg["content"]))
				if includeReasoning {
					reasoning.WriteString(stringValue(msg["reasoning_content"]))
				}
				appendToolCallDeltasFixed(msg["tool_calls"])
			}
		}
	}

	if id == "" {
		id = fmt.Sprintf("chatcmpl-codebuddy-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	message := gin.H{"role": "assistant", "content": content.String()}
	if includeReasoning && reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		// Drop truncated tool_call arguments when stream ended without [DONE]
		// or finish_reason=length (workbuddy2api Aggregate P1b).
		if finishReason == "length" || !sawDone {
			filtered := make([]map[string]any, 0, len(toolCalls))
			for _, call := range toolCalls {
				fn, _ := call["function"].(map[string]any)
				args := ""
				if fn != nil {
					args = stringValue(fn["arguments"])
				}
				if args != "" && !jsonLooksComplete(args) {
					continue
				}
				filtered = append(filtered, call)
			}
			toolCalls = filtered
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		} else if finishReason == "tool_calls" {
			finishReason = "stop"
		}
	}

	result := gin.H{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []gin.H{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		sanitizeCodebuddyUsage(usage)
		result["usage"] = ensureCodebuddyUsageTotal(usage)
	}
	return result, usageSnap
}

// jsonLooksComplete is a cheap completeness check for tool-call arguments.
func jsonLooksComplete(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return true
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) == nil
}

// ensureCodebuddyUsageTotal synthesizes total_tokens when the upstream omitted
// it but both prompt_tokens and completion_tokens are present.
func ensureCodebuddyUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := u["prompt_tokens"].(float64)
	ct, cok := u["completion_tokens"].(float64)
	if !pok || !cok {
		if pi, pok2 := u["prompt_tokens"].(int64); pok2 {
			if ci, cok2 := u["completion_tokens"].(int64); cok2 {
				out := make(map[string]any, len(u)+1)
				for k, v := range u {
					out[k] = v
				}
				out["total_tokens"] = pi + ci
				return out
			}
		}
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["total_tokens"] = pt + ct
	return out
}

func appendToolCallDeltas(raw any, target *[]map[string]any, indexes map[int]int) {
	deltas, _ := raw.([]any)
	for _, rawDelta := range deltas {
		delta, ok := rawDelta.(map[string]any)
		if !ok {
			continue
		}
		index := len(*target) - 1
		if value, ok := delta["index"].(float64); ok {
			index = int(value)
		}
		position, exists := indexes[index]
		if !exists {
			position = len(*target)
			indexes[index] = position
			*target = append(*target, map[string]any{
				"index": index,
				"type":  "function",
				"function": map[string]any{
					"name":      "",
					"arguments": "",
				},
			})
		}
		entry := (*target)[position]
		if id := strings.TrimSpace(stringValue(delta["id"])); id != "" {
			entry["id"] = id
		}
		if kind := strings.TrimSpace(stringValue(delta["type"])); kind != "" {
			entry["type"] = kind
		}
		function, _ := delta["function"].(map[string]any)
		if function == nil {
			continue
		}
		existing, _ := entry["function"].(map[string]any)
		if existing == nil {
			existing = map[string]any{"name": "", "arguments": ""}
			entry["function"] = existing
		}
		if name := stringValue(function["name"]); name != "" {
			existing["name"] = stringValue(existing["name"]) + name
		}
		if arguments := stringValue(function["arguments"]); arguments != "" {
			existing["arguments"] = stringValue(existing["arguments"]) + arguments
		}
	}
}

func sanitizeCodebuddyUsage(usage map[string]any) {
	for _, key := range []string{
		"credit", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens",
		"cache_read_input_tokens", "cache_creation_input_tokens",
		"prompt_cache_write_tokens", "completion_thinking_tokens", "cached_tokens",
	} {
		delete(usage, key)
	}
}
