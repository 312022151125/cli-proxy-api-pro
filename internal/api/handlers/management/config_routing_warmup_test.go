package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestRoutingWarmupHandlers(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	h := &Handler{cfg: cfg, configFilePath: writeTestConfigFile(t)}
	r := gin.New()
	r.GET("/routing/warmup", h.GetRoutingWarmup)
	r.PUT("/routing/warmup", h.PutRoutingWarmup)
	r.PATCH("/routing/warmup", h.PutRoutingWarmup)

	getReq := httptest.NewRequest(http.MethodGet, "/routing/warmup", nil)
	getRec := httptest.NewRecorder()
	r.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", getRec.Code, http.StatusOK)
	}
	if strings.TrimSpace(getRec.Body.String()) != `{"warmup":false}` {
		t.Fatalf("GET body = %s", getRec.Body.String())
	}

	putReq := httptest.NewRequest(http.MethodPut, "/routing/warmup", strings.NewReader(`{"value":true}`))
	putReq.Header.Set("Content-Type", "application/json")
	putRec := httptest.NewRecorder()
	r.ServeHTTP(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want %d; body=%s", putRec.Code, http.StatusOK, putRec.Body.String())
	}
	if !cfg.Routing.Warmup {
		t.Fatal("expected routing.warmup to become true")
	}

	patchReq := httptest.NewRequest(http.MethodPatch, "/routing/warmup", strings.NewReader(`{"value":false}`))
	patchReq.Header.Set("Content-Type", "application/json")
	patchRec := httptest.NewRecorder()
	r.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want %d; body=%s", patchRec.Code, http.StatusOK, patchRec.Body.String())
	}
	if cfg.Routing.Warmup {
		t.Fatal("expected routing.warmup to become false")
	}
}

func TestRunRoutingWarmup(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{Routing: config.RoutingConfig{Strategy: "fill-first", Warmup: true}}
	manager := coreauth.NewManager(nil, nil, nil)
	target := &coreauth.Auth{ID: "target-run-routing-warmup", Provider: "codex", Metadata: map[string]any{"email": "target@example.com"}}
	if _, err := manager.Register(context.Background(), target); err != nil {
		t.Fatalf("register target: %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(target.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4-mini"}})
	t.Cleanup(func() {
		reg.UnregisterClient(target.ID)
	})

	h := NewHandler(cfg, writeTestConfigFile(t), manager)
	calls := 0
	h.warmupListener.execute = func(context.Context, []string, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
		calls++
		return cliproxyexecutor.Response{}, nil
	}
	h.warmupListener.now = func() time.Time { return time.Unix(1000, 0) }

	r := gin.New()
	r.POST("/routing/warmup/run", h.RunRoutingWarmup)

	req := httptest.NewRequest(http.MethodPost, "/routing/warmup/run", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var body struct {
		Status    string   `json:"status"`
		Attempted int      `json:"attempted"`
		Succeeded int      `json:"succeeded"`
		Failed    []string `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("status = %q", body.Status)
	}
	if body.Attempted != 1 || body.Succeeded != 1 {
		t.Fatalf("attempted/succeeded = %d/%d, want 1/1", body.Attempted, body.Succeeded)
	}
	if len(body.Failed) != 0 {
		t.Fatalf("failed = %#v, want empty", body.Failed)
	}
	if calls != 1 {
		t.Fatalf("execute calls = %d, want 1", calls)
	}
}

func TestRunRoutingWarmupDisabled(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	h := NewHandler(&config.Config{Routing: config.RoutingConfig{Strategy: "round-robin", Warmup: false}}, writeTestConfigFile(t), coreauth.NewManager(nil, nil, nil))
	r := gin.New()
	r.POST("/routing/warmup/run", h.RunRoutingWarmup)

	req := httptest.NewRequest(http.MethodPost, "/routing/warmup/run", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}
