package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestCache(t *testing.T, items map[ConfigIdentity]CachedItem, version string) *ConfigCache {
	t.Helper()
	cache := NewConfigCache()
	cache.Replace(items, version)
	return cache
}

func TestHandlerHealth(t *testing.T) {
	cache := NewConfigCache()
	handler := NewConfigHandler(cache, ClientConfig{})

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body=%q, want ok", rec.Body.String())
	}
}

func TestHandlerVersion(t *testing.T) {
	cache := newTestCache(t, nil, "commit-xyz")
	handler := NewConfigHandler(cache, ClientConfig{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/version", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp versionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RepoVersion != "commit-xyz" {
		t.Fatalf("repo_version=%q, want commit-xyz", resp.RepoVersion)
	}
}

func TestHandlerMeta(t *testing.T) {
	cache := NewConfigCache()
	handler := NewConfigHandler(cache, ClientConfig{
		RefreshInterval: 30 * time.Second,
		MaxCacheTTL:     5 * time.Minute,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var resp MetaResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RefreshInterval != "30s" {
		t.Fatalf("refresh_interval=%q, want 30s", resp.RefreshInterval)
	}
	if resp.MaxCacheTTL != "5m0s" {
		t.Fatalf("max_cache_ttl=%q, want 5m0s", resp.MaxCacheTTL)
	}
}

func TestHandlerConfigReturnsValue(t *testing.T) {
	cache := newTestCache(t, map[ConfigIdentity]CachedItem{
		{Namespace: "payment", ConfigKey: "risk.yaml"}: {Value: "enabled: true"},
	}, "v1")
	handler := NewConfigHandler(cache, ClientConfig{})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/config?namespace=payment&key=risk.yaml", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if rec.Body.String() != "enabled: true" {
		t.Fatalf("body=%q", rec.Body.String())
	}
}

func TestHandlerConfigMissingReturns404(t *testing.T) {
	cache := NewConfigCache()
	handler := NewConfigHandler(cache, ClientConfig{})

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/config?namespace=ns&key=missing.yaml", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", rec.Code)
	}
}

func TestHandlerConfigMissingParamsReturns400(t *testing.T) {
	cache := NewConfigCache()
	handler := NewConfigHandler(cache, ClientConfig{})

	// 缺少 key 参数
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config?namespace=ns", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestHandlerSnapshot(t *testing.T) {
	cache := newTestCache(t, map[ConfigIdentity]CachedItem{
		{Namespace: "payment", ConfigKey: "risk.yaml"}:  {Value: "enabled: true"},
		{Namespace: "common", ConfigKey: "whitelist.yaml"}: {Value: "[a,b]"},
		{Namespace: "ns", ConfigKey: "deleted.yaml"}:     {Value: "", Deleted: true},
	}, "commit-123")
	handler := NewConfigHandler(cache, ClientConfig{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}

	var resp snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RepoVersion != "commit-123" {
		t.Fatalf("repo_version=%q", resp.RepoVersion)
	}
	// deleted 项不应出现在快照中
	if len(resp.Items) != 2 {
		t.Fatalf("items count=%d, want 2 (deleted excluded)", len(resp.Items))
	}

	found := make(map[string]string)
	for _, item := range resp.Items {
		found[item.Namespace+"/"+item.ConfigKey] = item.Value
	}
	if found["payment/risk.yaml"] != "enabled: true" {
		t.Fatalf("payment/risk.yaml=%q", found["payment/risk.yaml"])
	}
	if found["common/whitelist.yaml"] != "[a,b]" {
		t.Fatalf("common/whitelist.yaml=%q", found["common/whitelist.yaml"])
	}
}

func TestHandlerSnapshotEmpty(t *testing.T) {
	cache := NewConfigCache()
	handler := NewConfigHandler(cache, ClientConfig{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/snapshot", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp snapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 0 {
		t.Fatalf("items count=%d, want 0", len(resp.Items))
	}
}
