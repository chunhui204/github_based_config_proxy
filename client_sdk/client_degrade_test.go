package client_sdk

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestIsDegradedFalseAfterSuccessfulInit(t *testing.T) {
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]ConfigItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "a"},
			},
		},
	}
	client := newTestClient(t, store)
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.IsDegraded() {
		t.Fatal("IsDegraded should be false after successful init")
	}
	if err := client.LastRefreshError(); err != nil {
		t.Fatalf("LastRefreshError should be nil, got %v", err)
	}
}

func TestIsDegradedTrueAfterRefreshFailure(t *testing.T) {
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]ConfigItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "old"},
			},
		},
		versionErr: errors.New("server unreachable"),
	}
	client := newTestClient(t, store)
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	makeVersionCheckExpired(client)

	// 刷新失败
	if err := client.refresh(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}

	if !client.IsDegraded() {
		t.Fatal("IsDegraded should be true after refresh failure")
	}
	if client.LastRefreshError() == nil {
		t.Fatal("LastRefreshError should be non-nil")
	}
}

func TestIsDegradedRecoversAfterSuccessfulRefresh(t *testing.T) {
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]ConfigItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "old"},
			},
		},
		nextVersion: "v2",
		nextSnapshot: Snapshot{
			RepoVersion: "v2",
			Items: map[ConfigIdentity]ConfigItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "new"},
			},
		},
	}
	client := newTestClient(t, store)
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 第一次刷新：版本变化，成功
	makeVersionCheckExpired(client)
	if err := client.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.IsDegraded() {
		t.Fatal("IsDegraded should be false after successful refresh")
	}

	// 模拟故障
	store.versionErr = errors.New("network error")
	makeVersionCheckExpired(client)
	_ = client.refresh(context.Background())
	if !client.IsDegraded() {
		t.Fatal("IsDegraded should be true during failure")
	}

	// 恢复
	store.versionErr = nil
	makeVersionCheckExpired(client)
	if err := client.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.IsDegraded() {
		t.Fatal("IsDegraded should be false after recovery")
	}
	if client.LastRefreshError() != nil {
		t.Fatalf("LastRefreshError should be nil after recovery, got %v", client.LastRefreshError())
	}
}

func TestDegradedKeepsOldCacheValues(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: "stable-config"},
			},
		},
		versionErr: errors.New("all servers down"),
	}
	client := newTestClient(t, store)
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 多次刷新失败，缓存值必须保留
	for i := 0; i < 5; i++ {
		makeVersionCheckExpired(client)
		_ = client.refresh(context.Background())
	}

	value, ok := client.GetConfigOK("payment", "risk.yaml")
	if !ok || value != "stable-config" {
		t.Fatalf("value=%q ok=%v, want stable-config/true", value, ok)
	}
}

func TestDegradedTypedConfigKeepsOldObject(t *testing.T) {
	type RiskConfig struct {
		Enabled bool `json:"enabled"`
		Limit   int  `json:"limit"`
	}
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.json"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: `{"enabled":true,"limit":100}`},
			},
		},
		versionErr: errors.New("server down"),
	}
	client := newTestClient(t, store)
	riskCfg := Register[RiskConfig](client, "payment", "risk.json")
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 刷新失败
	makeVersionCheckExpired(client)
	_ = client.refresh(context.Background())

	val, ok := riskCfg.Get()
	if !ok || !val.Enabled || val.Limit != 100 {
		t.Fatalf("typed config during degradation: %+v ok=%v", val, ok)
	}
}

func TestDegradedBeforeInitReturnsFalse(t *testing.T) {
	store := &fakeClientStore{}
	client := newTestClient(t, store)

	// 未 Init 时，IsDegraded 应为 false（initialized=false 即使有错误也不算降级）
	if client.IsDegraded() {
		t.Fatal("IsDegraded should be false before Init")
	}
}

func TestNewClientWithServer(t *testing.T) {
	// 使用 httptest server 验证端到端连通
	server := newMockServer(allEndpointsHandler("v1", []map[string]string{
		{"namespace": "ns", "config_key": "a.yaml", "value": "hello"},
	}))
	defer server.Close()

	client, err := NewClientWithServer([]string{server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Init(ctx); err != nil {
		t.Fatal(err)
	}

	value, ok := client.GetConfigOK("ns", "a.yaml")
	if !ok || value != "hello" {
		t.Fatalf("value=%q ok=%v", value, ok)
	}
	if client.IsDegraded() {
		t.Fatal("should not be degraded")
	}
}

func TestNewClientWithServerFailover(t *testing.T) {
	// server1 关闭
	badServer := newMockServer(func(w http.ResponseWriter, r *http.Request) {})
	badServer.Close()
	// server2 正常
	goodServer := newMockServer(allEndpointsHandler("v1", []map[string]string{
		{"namespace": "ns", "config_key": "b.yaml", "value": "from-good"},
	}))
	defer goodServer.Close()

	client, err := NewClient(Config{
		ServerAddrs: []string{badServer.URL, goodServer.URL},
		HTTPTimeout: time.Second,
		FailBackoff: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Init(ctx); err != nil {
		t.Fatal(err)
	}

	value, ok := client.GetConfigOK("ns", "b.yaml")
	if !ok || value != "from-good" {
		t.Fatalf("value=%q ok=%v", value, ok)
	}
}
