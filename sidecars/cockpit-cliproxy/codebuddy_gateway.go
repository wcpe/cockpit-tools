package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
func codebuddyCatalogForAPIKey(m *manifest, spec *apiKeySpec) []string {
	upstreams := codebuddyUpstreamsForAPIKey(m, spec)
	models := make([]string, 0)
	seen := make(map[string]struct{})
	anyAccountCatalog := false
	for _, upstream := range upstreams {
		if len(upstream.ModelIDs) == 0 {
			continue
		}
		anyAccountCatalog = true
		for _, model := range upstream.ModelIDs {
			key := strings.ToLower(model)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			models = append(models, model)
		}
	}
	if !anyAccountCatalog {
		models = append(models, codebuddyDefaultModelIDs...)
	}
	if prefix := strings.Trim(strings.TrimSpace(spec.ModelPrefix), "/"); prefix != "" {
		prefixed := make([]string, 0, len(models))
		for _, model := range models {
			prefixed = append(prefixed, prefix+"/"+model)
		}
		return prefixed
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
	c.JSON(http.StatusOK, codebuddyModelsResponse(codebuddyCatalogForAPIKey(s.manifest, spec)))
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

	catalog := codebuddyCatalogForAPIKey(s.manifest, spec)
	if len(catalog) > 0 && !stringSliceContainsFold(catalog, model) {
		writeAPIError(c, http.StatusNotFound, fmt.Sprintf("model %s not found", model), "model_not_found")
		return
	}

	upstreamBody, err := codebuddyRequestBody(clientBody, model)
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, "failed to encode request body", "invalid_request")
		return
	}
	clientWantsStream := requestBodyStream(clientBody)

	order := codebuddySelectionOrder(s.manifest, upstreams)
	var lastStatus int
	var lastMessage string
	for _, upstream := range order {
		if !codebuddyUpstreamSupportsModel(upstream, model) {
			continue
		}
		status, message, resp := s.openCodebuddyStream(c, upstream, upstreamBody, model)
		if resp != nil {
			defer resp.Body.Close()
			s.writeCodebuddyResponse(c, resp, model, clientWantsStream, requestedReasoning(upstream))
			return
		}
		lastStatus, lastMessage = status, message
	}

	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	if strings.TrimSpace(lastMessage) == "" {
		lastMessage = "all CodeBuddy upstream accounts failed"
	}
	writeAPIError(c, codebuddyHTTPStatus(lastStatus), lastMessage, "upstream_error")
}

// codebuddySelectionOrder applies the manifest routing strategy and lets a
// failed account fall through to the next one.
func codebuddySelectionOrder(m *manifest, upstreams []*codebuddyUpstreamSpec) []*codebuddyUpstreamSpec {
	if len(upstreams) <= 1 {
		return upstreams
	}
	if m != nil && strings.EqualFold(strings.TrimSpace(m.RoutingStrategy), "random") {
		offset := int(codebuddyRotationCounter.Add(1) % uint64(len(upstreams)))
		order := make([]*codebuddyUpstreamSpec, 0, len(upstreams))
		order = append(order, upstreams[offset:]...)
		order = append(order, upstreams[:offset]...)
		return order
	}
	offset := int((codebuddyRotationCounter.Add(1) - 1) % uint64(len(upstreams)))
	order := make([]*codebuddyUpstreamSpec, 0, len(upstreams))
	order = append(order, upstreams[offset:]...)
	order = append(order, upstreams[:offset]...)
	return order
}

func requestedReasoning(upstream *codebuddyUpstreamSpec) bool {
	return upstream != nil && upstream.IncludeReasoning
}

// openCodebuddyStream performs the upstream call. It returns (status, message,
// nil) when the upstream rejected the request, so the caller can try the next
// account. A non-nil response is always HTTP 200 with an open stream.
func (s *relayServer) openCodebuddyStream(c *gin.Context, upstream *codebuddyUpstreamSpec, body []byte, model string) (int, string, *http.Response) {
	startedAt := time.Now()
	resp, err := s.doCodebuddyRequest(c, upstream, upstream.AccessToken, body)
	if err != nil {
		s.emitExecutorDiagnostic(c, "codebuddy_request_failed", model, upstream.ID, startedAt, err.Error())
		return http.StatusBadGateway, err.Error(), nil
	}

	if resp.StatusCode == http.StatusUnauthorized && strings.TrimSpace(upstream.RefreshToken) != "" {
		_ = resp.Body.Close()
		refreshed, refreshErr := refreshCodebuddyToken(c, upstream)
		if refreshErr != nil {
			s.emitExecutorDiagnostic(c, "codebuddy_token_refresh_failed", model, upstream.ID, startedAt, refreshErr.Error())
			return http.StatusUnauthorized, "CodeBuddy credential expired and could not be refreshed", nil
		}
		resp, err = s.doCodebuddyRequest(c, upstream, refreshed, body)
		if err != nil {
			return http.StatusBadGateway, err.Error(), nil
		}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		status, message := codebuddyUpstreamError(resp)
		s.emitExecutorDiagnostic(c, "codebuddy_upstream_error", model, upstream.ID, startedAt,
			fmt.Sprintf("status=%d message=%s", status, message))
		return status, message, nil
	}
	return http.StatusOK, "", resp
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
	intent := strings.TrimSpace(upstream.AgentIntent)
	if intent == "" {
		intent = codebuddyDefaultAgentIntent
	}
	req.Header.Set("X-Agent-Intent", intent)
	copyProviderGatewayDiagnosticHeaders(req.Header, c.Request.Header)
	return http.DefaultClient.Do(req)
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

// globalCodebuddyEmitter mirrors the process emitter so a refresh that happens
// on a request path can still report back to Cockpit. main() assigns it.
var globalCodebuddyEmitter *eventEmitter

// codebuddyRequestBody forces streaming and drops fields the gateway rejects.
func codebuddyRequestBody(clientBody []byte, model string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(clientBody, &payload); err != nil {
		return nil, err
	}
	payload["model"] = model
	payload["stream"] = true
	delete(payload, "stream_options")
	delete(payload, "logprobs")
	delete(payload, "top_logprobs")
	if value, ok := payload["n"].(float64); ok && value > 1 {
		delete(payload, "n")
	}
	if _, ok := payload["max_tokens"]; !ok {
		if value, ok := payload["max_completion_tokens"]; ok {
			payload["max_tokens"] = value
		}
	}
	delete(payload, "max_completion_tokens")
	return json.Marshal(payload)
}

// codebuddyUpstreamError converts an upstream error body into a client-facing
// message plus the HTTP status the relay should surface.
func codebuddyUpstreamError(resp *http.Response) (int, string) {
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	message := strings.TrimSpace(string(payload))
	var decoded struct {
		Code       json.RawMessage `json:"code"`
		Msg        string          `json:"msg"`
		ErrorMsg   string          `json:"error_msg"`
		DisplayMsg struct {
			En string `json:"en"`
			Zh string `json:"zh"`
		} `json:"displayMsg"`
	}
	if err := json.Unmarshal(payload, &decoded); err == nil {
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
		message = fmt.Sprintf("CodeBuddy upstream returned status %d", resp.StatusCode)
	}
	return codebuddyHTTPStatus(resp.StatusCode), message
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
func (s *relayServer) writeCodebuddyResponse(c *gin.Context, resp *http.Response, model string, clientWantsStream, includeReasoning bool) {
	if !clientWantsStream {
		c.JSON(http.StatusOK, codebuddyAggregateResponse(resp.Body, model, includeReasoning))
		return
	}

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		writeAPIError(c, http.StatusInternalServerError, "streaming not supported", "streaming_not_supported")
		return
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
			return
		}
		chunk, err := decodeCodebuddyChunk(payload)
		if err != nil {
			continue
		}
		if message, isError := codebuddyInlineError(chunk); isError {
			_, _ = c.Writer.Write(codebuddyStreamErrorFrame(message))
			flusher.Flush()
			return
		}
		sanitizeCodebuddyChunk(chunk, model, includeReasoning)
		encoded, err := json.Marshal(chunk)
		if err != nil {
			continue
		}
		if _, err := c.Writer.Write([]byte("data: " + string(encoded) + "\n\n")); err != nil {
			return
		}
		flusher.Flush()
	}
	if err := scanner.Err(); err != nil {
		_, _ = c.Writer.Write(codebuddyStreamErrorFrame("CodeBuddy stream interrupted: " + err.Error()))
		flusher.Flush()
	}
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

// codebuddyAggregateResponse folds the upstream stream into one
// `chat.completion` object for clients that requested `stream: false`.
func codebuddyAggregateResponse(body io.Reader, model string, includeReasoning bool) gin.H {
	var (
		id           string
		created      int64
		content      strings.Builder
		reasoning    strings.Builder
		finishReason = "stop"
		usage        map[string]any
		toolCalls    []map[string]any
		toolIndexes  = make(map[int]int)
	)

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
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
			}}
		}
		if value := strings.TrimSpace(stringValue(chunk["id"])); value != "" {
			id = value
		}
		if value, ok := chunk["created"].(float64); ok && value > 0 {
			created = int64(value)
		}
		if value, ok := chunk["usage"].(map[string]any); ok && value != nil {
			usage = value
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
			delta, ok := choice["delta"].(map[string]any)
			if !ok {
				continue
			}
			content.WriteString(stringValue(delta["content"]))
			if includeReasoning {
				reasoning.WriteString(stringValue(delta["reasoning_content"]))
			}
			appendToolCallDeltas(delta["tool_calls"], &toolCalls, toolIndexes)
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
		message["tool_calls"] = toolCalls
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
		result["usage"] = usage
	}
	return result
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
