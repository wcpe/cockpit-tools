package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestCodebuddyPoolE2ESelectionAndLedger — mock dual upstreams, verify free-first
// pick, request ledger persistence, and /v1/codebuddy/status pool payload.
func TestCodebuddyPoolE2ESelectionAndLedger(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	t.Setenv("COCKPIT_CODEBUDDY_STATE_DIR", dir)
	// Reset process-local singletons so they load from the temp dir.
	codebuddyPoolOnce = sync.Once{}
	codebuddyPoolInst = nil
	codebuddyGov = nil
	codebuddyReqLog = nil
	codebuddyAffinity = &codebuddySessionAffinity{binds: map[string]codebuddySessionBind{}}

	freeCredit := int64(5)
	paidCredit := int64(999999)

	freeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"credit\":0}}\n\ndata: [DONE]\n\n"))
	}))
	defer freeUp.Close()
	paidUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c2\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15,\"credit\":2.5}}\n\ndata: [DONE]\n\n"))
	}))
	defer paidUp.Close()

	apiKey := codebuddyTestAPIKey("acct_free", "acct_paid")
	m := newCodebuddyTestManifest(apiKey,
		codebuddyUpstreamSpec{ID: "acct_free", BaseURL: freeUp.URL, AccessToken: "jwt-free", ModelIDs: []string{"hy3"}, Realm: "cn", Credits: &freeCredit},
		codebuddyUpstreamSpec{ID: "acct_paid", BaseURL: paidUp.URL, AccessToken: "jwt-paid", ModelIDs: []string{"hy3"}, Realm: "cn", Credits: &paidCredit},
	)
	srv := newCodebuddyTestServer(m)

	// Seed cost ledger: free account free, paid account paid.
	pool := codebuddyPoolInit()
	pool.SyncFromManifest(m.CodebuddyUpstreams)
	pool.NoteModelCost("acct_free", "hy3", 0, 15)
	pool.NoteModelCost("acct_paid", "hy3", 2.5, 15)

	// Chat through the real router — should prefer free after cost knowledge.
	body := []byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer client-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
	}
	var chat map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &chat); err != nil {
		t.Fatalf("chat json: %v", err)
	}
	if chat["model"] != "hy3" {
		t.Fatalf("model=%v", chat["model"])
	}

	// Ledger must contain at least the successful chat attempt.
	recs, total := codebuddyRequestLogInit().ListPage(0, 20, "")
	if total < 1 {
		t.Fatalf("request ledger empty")
	}
	foundOK := false
	for _, rec := range recs {
		if rec.Outcome == "ok" && rec.Model == "hy3" {
			foundOK = true
			if rec.AccountID != "acct_free" && rec.AccountID != "acct_paid" {
				t.Fatalf("unexpected account in ledger: %+v", rec)
			}
		}
	}
	if !foundOK {
		t.Fatalf("no ok record in ledger: %+v", recs)
	}
	if _, err := os.Stat(filepath.Join(dir, codebuddyRequestLogName)); err != nil {
		t.Fatalf("request log file missing: %v", err)
	}

	// Status exposes pool ledger.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/codebuddy/status", nil)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", w2.Code, w2.Body.String())
	}
	var status map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &status); err != nil {
		t.Fatalf("status json: %v body=%s", err, w2.Body.String())
	}
	poolVal, ok := status["pool"].(map[string]any)
	if !ok {
		t.Fatalf("status missing pool: keys=%v", status)
	}
	accounts, _ := poolVal["accounts"].([]any)
	if len(accounts) < 2 {
		t.Fatalf("pool accounts=%v", accounts)
	}

	// Selection order after cost knowledge: free first.
	// Clear pick-gap timestamps so anti-herd does not hide the free account
	// immediately after the live chat request.
	pool.mu.Lock()
	for _, e := range pool.entries {
		e.LastUsed = time.Time{}
	}
	pool.mu.Unlock()
	order := pool.PickOrder(m.CodebuddyUpstreamsPointers(), "hy3", "", "")
	if len(order) == 0 || order[0].ID != "acct_free" {
		var ids []string
		for _, u := range order {
			ids = append(ids, u.ID)
		}
		t.Fatalf("pick order=%v want acct_free first", ids)
	}

	// Models endpoint still works.
	req3 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req3.Header.Set("Authorization", "Bearer client-key")
	w3 := httptest.NewRecorder()
	srv.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK || !strings.Contains(w3.Body.String(), "hy3") {
		t.Fatalf("models code=%d body=%s", w3.Code, w3.Body.String())
	}
}

func (m *manifest) CodebuddyUpstreamsPointers() []*codebuddyUpstreamSpec {
	out := make([]*codebuddyUpstreamSpec, 0, len(m.CodebuddyUpstreams))
	for i := range m.CodebuddyUpstreams {
		out = append(out, &m.CodebuddyUpstreams[i])
	}
	return out
}
