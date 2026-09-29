package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func codebuddyTestAPIKey(upstreamIDs ...string) *apiKeySpec {
	return &apiKeySpec{
		ID:           "cb_key",
		Label:        "CodeBuddy",
		Key:          "client-key",
		UpstreamKind: codebuddyUpstreamKind,
		AccountIDs:   upstreamIDs,
		Enabled:      true,
	}
}

func newCodebuddyTestManifest(apiKey *apiKeySpec, upstreams ...codebuddyUpstreamSpec) *manifest {
	m := &manifest{
		APIKeys:            []apiKeySpec{*apiKey},
		CodebuddyUpstreams: upstreams,
		apiKeyByValue:      map[string]*apiKeySpec{"client-key": apiKey},
		codebuddyByID:      map[string]*codebuddyUpstreamSpec{},
	}
	for i := range m.CodebuddyUpstreams {
		upstream := &m.CodebuddyUpstreams[i]
		normalizeCodebuddyUpstream(upstream)
		m.codebuddyByID[upstream.ID] = upstream
	}
	return m
}

func newCodebuddyTestServer(m *manifest) *gin.Engine {
	return (&relayServer{
		runtime:  &fakeRuntime{},
		cfg:      &config.Config{},
		manifest: m,
		policy:   &requestPolicy{manifest: m},
	}).router()
}

func TestLoadManifestNormalizesCodebuddySection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	payload := `{
	  "apiKeys": [{"id":"cb_key","label":"cb","key":"client-key","enabled":true,"upstreamKind":"workbuddy"}],
	  "codebuddyUpstreams": [
	    {"id":"acct_1","platform":"WorkBuddy","baseUrl":"https://copilot.tencent.com/","accessToken":"  tok  ","modelIds":["glm-5.1","glm-5.1",""]}
	  ]
	}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	m, err := loadManifest(path)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	spec := m.apiKeyByValue["client-key"]
	if spec == nil {
		t.Fatal("api key was not indexed")
	}
	if spec.UpstreamKind != codebuddyUpstreamKind {
		t.Fatalf("upstream kind = %q, want %q", spec.UpstreamKind, codebuddyUpstreamKind)
	}
	upstream := m.codebuddyByID["acct_1"]
	if upstream == nil {
		t.Fatal("codebuddy upstream was not indexed")
	}
	if upstream.BaseURL != "https://copilot.tencent.com" {
		t.Fatalf("base url = %q", upstream.BaseURL)
	}
	if upstream.AccessToken != "tok" {
		t.Fatalf("access token = %q", upstream.AccessToken)
	}
	if upstream.UserAgent != codebuddyDefaultUserAgent {
		t.Fatalf("default user agent was not applied: %q", upstream.UserAgent)
	}
	if len(upstream.ModelIDs) != 1 || upstream.ModelIDs[0] != "glm-5.1" {
		t.Fatalf("model ids = %#v", upstream.ModelIDs)
	}
}

func TestCodebuddyCatalogAndScope(t *testing.T) {
	apiKey := codebuddyTestAPIKey("acct_1")
	m := newCodebuddyTestManifest(apiKey,
		codebuddyUpstreamSpec{ID: "acct_1", BaseURL: "https://example.test", AccessToken: "a", ModelIDs: []string{"glm-5.1"}},
		codebuddyUpstreamSpec{ID: "acct_2", BaseURL: "https://example.test", AccessToken: "b", ModelIDs: []string{"kimi-k2.6"}},
	)
	catalog := codebuddyCatalogForAPIKey(m, apiKey)
	if len(catalog) != 1 || catalog[0] != "glm-5.1" {
		t.Fatalf("scoped catalog = %#v", catalog)
	}

	scoped := codebuddyTestAPIKey()
	scoped.ModelPrefix = "cb"
	all := codebuddyCatalogForAPIKey(m, scoped)
	if len(all) != 2 {
		t.Fatalf("unscoped catalog = %#v", all)
	}
	for _, model := range all {
		if !strings.HasPrefix(model, "cb/") {
			t.Fatalf("model prefix was not applied: %#v", all)
		}
	}

	// An upstream without a catalog falls back to the published list.
	fallback := codebuddyTestAPIKey("acct_3")
	m2 := newCodebuddyTestManifest(fallback,
		codebuddyUpstreamSpec{ID: "acct_3", BaseURL: "https://example.test", AccessToken: "c"},
	)
	if got := codebuddyCatalogForAPIKey(m2, fallback); len(got) != len(codebuddyDefaultModelIDs) {
		t.Fatalf("fallback catalog = %#v", got)
	}
}

func codebuddySSEBody() string {
	return strings.Join([]string{
		`data: {"id":"cmb-1","model":"ep-1","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"你","reasoning_content":"think","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}`,
		`data: {"id":"cmb-1","model":"ep-1","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"content":"好。","reasoning_content":"","function_call":{"name":"","arguments":""},"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}`,
		`data: {"id":"cmb-1","model":"ep-1","object":"chat.completion.chunk","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","function_call":{"name":"","arguments":""},"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8,"credit":0.12,"prompt_cache_hit_tokens":1,"cached_tokens":0}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
}

func TestCodebuddyStreamRewritesModelAndStripsGatewayFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var gotPath, gotAuth, gotIntent, gotUA string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotIntent = r.Header.Get("X-Agent-Intent")
		gotUA = r.Header.Get("User-Agent")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codebuddySSEBody()))
	}))
	defer upstream.Close()

	apiKey := codebuddyTestAPIKey("acct_1")
	m := newCodebuddyTestManifest(apiKey,
		codebuddyUpstreamSpec{ID: "acct_1", BaseURL: upstream.URL, AccessToken: "jwt-token"},
	)
	router := newCodebuddyTestServer(m)

	body := `{"model":"glm-5.1","stream":true,"max_completion_tokens":64,"n":2,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != codebuddyChatCompletionsPath {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotAuth != "Bearer jwt-token" {
		t.Fatalf("upstream auth = %q", gotAuth)
	}
	if gotIntent != codebuddyDefaultAgentIntent {
		t.Fatalf("upstream intent = %q", gotIntent)
	}
	if !strings.Contains(gotUA, "Chrome/") {
		t.Fatalf("upstream user agent = %q", gotUA)
	}
	if gotBody["stream"] != true {
		t.Fatalf("upstream stream flag = %#v", gotBody["stream"])
	}
	if gotBody["max_tokens"] != float64(64) {
		t.Fatalf("max_completion_tokens was not mapped: %#v", gotBody["max_tokens"])
	}
	if _, ok := gotBody["max_completion_tokens"]; ok {
		t.Fatal("max_completion_tokens should be dropped")
	}
	// Gateway injects stream_options.include_usage so the last SSE frame carries credit/tokens.
	so, _ := gotBody["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage should be true: %#v", gotBody["stream_options"])
	}
	if _, ok := gotBody["n"]; ok {
		t.Fatal("n>1 should be dropped")
	}

	out := rec.Body.String()
	if !strings.Contains(out, `"model":"glm-5.1"`) {
		t.Fatalf("response model was not rewritten: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("missing terminator: %s", out)
	}
	if strings.Contains(out, "extra_fields") || strings.Contains(out, `"credit"`) ||
		strings.Contains(out, "prompt_cache_hit_tokens") {
		t.Fatalf("gateway-only fields leaked: %s", out)
	}
	if strings.Contains(out, "reasoning_content") {
		t.Fatalf("reasoning_content should be stripped by default: %s", out)
	}
	if strings.Contains(out, `"function_call"`) {
		t.Fatalf("empty function_call should be stripped: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish reason missing: %s", out)
	}
}

func TestCodebuddyNonStreamIsSynthesized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		if payload["stream"] != true {
			t.Errorf("upstream stream = %#v, want true", payload["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codebuddySSEBody()))
	}))
	defer upstream.Close()

	apiKey := codebuddyTestAPIKey("acct_1")
	m := newCodebuddyTestManifest(apiKey,
		codebuddyUpstreamSpec{ID: "acct_1", BaseURL: upstream.URL, AccessToken: "jwt"},
	)
	router := newCodebuddyTestServer(m)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.1","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content type = %q", ct)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("expected a JSON body, got %q", rec.Body.String())
	}
	if decoded["object"] != "chat.completion" {
		t.Fatalf("object = %#v", decoded["object"])
	}
	if decoded["model"] != "glm-5.1" {
		t.Fatalf("model = %#v", decoded["model"])
	}
	choices, _ := decoded["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %#v", decoded["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["content"] != "你好。" {
		t.Fatalf("content = %#v", message["content"])
	}
	usage, _ := decoded["usage"].(map[string]any)
	if usage == nil || usage["total_tokens"] != float64(8) {
		t.Fatalf("usage = %#v", decoded["usage"])
	}
	if _, ok := usage["credit"]; ok {
		t.Fatalf("usage leaked gateway fields: %#v", usage)
	}
}

func TestCodebuddyAggregatesToolCallFragments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"ep","choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_","arguments":"{\"ci"},"index":0}]},"finish_reason":""}]}`,
		`data: {"id":"c1","model":"ep","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"name":"weather","arguments":"ty\":\"北京\"}"},"index":0}]},"finish_reason":""}]}`,
		`data: {"id":"c1","model":"ep","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	}))
	defer upstream.Close()

	apiKey := codebuddyTestAPIKey("acct_1")
	m := newCodebuddyTestManifest(apiKey,
		codebuddyUpstreamSpec{ID: "acct_1", BaseURL: upstream.URL, AccessToken: "jwt"},
	)
	router := newCodebuddyTestServer(m)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.1","stream":false,"messages":[{"role":"user","content":"weather"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body: %s", rec.Body.String())
	}
	choices, _ := decoded["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v", message["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	function, _ := call["function"].(map[string]any)
	if call["id"] != "call_1" {
		t.Fatalf("tool call id = %#v", call["id"])
	}
	if function["name"] != "get_weather" {
		t.Fatalf("tool name = %#v", function["name"])
	}
	if function["arguments"] != `{"city":"北京"}` {
		t.Fatalf("tool arguments = %#v", function["arguments"])
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish reason = %#v", choice["finish_reason"])
	}
}

func TestCodebuddyUpstreamErrorIsMapped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":11102,"msg":"model [x] service info not found","displayMsg":{"en":"The requested model is not available.","zh":"请求的模型不可用"}}`))
	}))
	defer upstream.Close()

	apiKey := codebuddyTestAPIKey("acct_1")
	m := newCodebuddyTestManifest(apiKey,
		codebuddyUpstreamSpec{ID: "acct_1", BaseURL: upstream.URL, AccessToken: "jwt"},
	)
	router := newCodebuddyTestServer(m)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body: %s", rec.Body.String())
	}
	errPayload, _ := decoded["error"].(map[string]any)
	if errPayload == nil || !strings.Contains(fmt.Sprint(errPayload["message"]), "模型") {
		t.Fatalf("error payload = %#v", decoded)
	}
}

func TestCodebuddyFallsBackToNextAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var hits []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		hits = append(hits, token)
		if token == "bad" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":10002,"msg":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	apiKey := codebuddyTestAPIKey("acct_1", "acct_2")
	m := newCodebuddyTestManifest(apiKey,
		codebuddyUpstreamSpec{ID: "acct_1", BaseURL: upstream.URL, AccessToken: "bad"},
		codebuddyUpstreamSpec{ID: "acct_2", BaseURL: upstream.URL, AccessToken: "good"},
	)
	router := newCodebuddyTestServer(m)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer client-key")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d body = %s", i, rec.Code, rec.Body.String())
		}
	}
	// The first request must probe the bad account, then succeed on the good
	// one. Round-robin then starts the second request on the good account.
	if len(hits) < 3 {
		t.Fatalf("expected a fallback probe, got %v", hits)
	}
	if hits[0] != "bad" || hits[1] != "good" {
		t.Fatalf("unexpected probe order: %v", hits)
	}
}

func TestCodebuddyConversationKeyStickyFallbacks(t *testing.T) {
	// prompt_cache_key
	payload := map[string]any{"prompt_cache_key": "sess-abc", "messages": []any{}}
	if k := codebuddyConversationKeyFrom(nil, payload); k != "pck:sess-abc" {
		t.Fatalf("pck key=%q", k)
	}
	// user_id suppresses hist fallback
	payload2 := map[string]any{
		"user_id": "u1",
		"messages": []any{
			map[string]any{"role": "user", "content": "hello sticky"},
		},
	}
	if k := codebuddyConversationKeyFrom(nil, payload2); k != "" {
		t.Fatalf("user_id should suppress sticky, got %q", k)
	}
	// hist fallback without user_id
	payload3 := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello sticky"},
		},
	}
	if k := codebuddyConversationKeyFrom(nil, payload3); !strings.HasPrefix(k, "hist:") {
		t.Fatalf("hist key=%q", k)
	}
	// image content still produces a signature
	payload4 := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
				},
			},
		},
	}
	if k := codebuddyConversationKeyFrom(nil, payload4); !strings.HasPrefix(k, "hist:") {
		t.Fatalf("image hist key=%q", k)
	}
	// turn-level request id rotates with turn text
	id1 := deriveTurnRequestID("conv:x", "u1:first")
	id2 := deriveTurnRequestID("conv:x", "u1:second")
	id3 := deriveTurnRequestID("conv:x", "u1:first")
	if id1 == "" || id1 == id2 || id1 != id3 {
		t.Fatalf("turn ids %s %s %s", id1, id2, id3)
	}
}

func TestCodebuddyModelsEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	apiKey := codebuddyTestAPIKey("acct_1")
	m := newCodebuddyTestManifest(apiKey,
		codebuddyUpstreamSpec{ID: "acct_1", BaseURL: "https://example.test", AccessToken: "a", ModelIDs: []string{"glm-5.1", "kimi-k2.6"}},
	)
	router := newCodebuddyTestServer(m)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var decoded struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("body: %s", rec.Body.String())
	}
	if len(decoded.Data) != 2 {
		t.Fatalf("models = %#v", decoded.Data)
	}
	for _, entry := range decoded.Data {
		if entry["owned_by"] != "codebuddy" {
			t.Fatalf("owned_by = %#v", entry["owned_by"])
		}
	}
}

func TestCodebuddyRejectsResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	apiKey := codebuddyTestAPIKey("acct_1")
	m := newCodebuddyTestManifest(apiKey,
		codebuddyUpstreamSpec{ID: "acct_1", BaseURL: "https://example.test", AccessToken: "a"},
	)
	router := newCodebuddyTestServer(m)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"glm-5.1","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer client-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}
