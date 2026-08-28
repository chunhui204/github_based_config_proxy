package client_sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newMockServer 创建一个模拟 Config Server 的 httptest.Server。
// handler 用于自定义各路径的响应行为。
func newMockServer(handler http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(handler)
}

// versionHandler 返回固定版本号的 handler。
func versionHandler(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]string{"repo_version": version})
	}
}

// snapshotHandler 返回固定快照的 handler。
func snapshotHandler(version string, items []map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/snapshot" {
			http.NotFound(w, r)
			return
		}
		type itemDTO struct {
			Namespace string `json:"namespace"`
			ConfigKey string `json:"config_key"`
			Value     string `json:"value"`
			Deleted   bool   `json:"deleted"`
		}
		dtos := make([]itemDTO, 0, len(items))
		for _, item := range items {
			dtos = append(dtos, itemDTO{
				Namespace: item["namespace"],
				ConfigKey: item["config_key"],
				Value:     item["value"],
			})
		}
		writeJSON(w, map[string]any{"repo_version": version, "items": dtos})
	}
}

// metaHandler 返回 meta 配置的 handler。
func metaHandler(refreshInterval, maxCacheTTL string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/meta" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]string{
			"refresh_interval": refreshInterval,
			"max_cache_ttl":    maxCacheTTL,
		})
	}
}

// allEndpointsHandler 将 version/snapshot/meta 组合到一个 server。
func allEndpointsHandler(version string, items []map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/version":
			versionHandler(version).ServeHTTP(w, r)
		case "/api/v1/snapshot":
			snapshotHandler(version, items).ServeHTTP(w, r)
		case "/api/v1/meta":
			metaHandler("30s", "5m").ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestHTTPStoreGetRepoVersion(t *testing.T) {
	server := newMockServer(versionHandler("commit-abc"))
	defer server.Close()

	store := NewHTTPStore([]string{server.URL}, time.Second, time.Minute)
	version, err := store.GetRepoVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "commit-abc" {
		t.Fatalf("version=%q, want commit-abc", version)
	}
}

func TestHTTPStoreLoadSnapshot(t *testing.T) {
	items := []map[string]string{
		{"namespace": "payment", "config_key": "risk.json", "value": `{"enabled":true}`},
		{"namespace": "common", "config_key": "list.json", "value": `[1,2,3]`},
	}
	server := newMockServer(snapshotHandler("v1", items))
	defer server.Close()

	store := NewHTTPStore([]string{server.URL}, time.Second, time.Minute)
	snapshot, err := store.LoadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RepoVersion != "v1" {
		t.Fatalf("version=%q", snapshot.RepoVersion)
	}
	if len(snapshot.Items) != 2 {
		t.Fatalf("items count=%d, want 2", len(snapshot.Items))
	}
	val := snapshot.Items[ConfigIdentity{Namespace: "payment", ConfigKey: "risk.json"}]
	if val.Value != `{"enabled":true}` {
		t.Fatalf("value=%q", val.Value)
	}
}

func TestHTTPStoreLoadMetaConfig(t *testing.T) {
	server := newMockServer(metaHandler("45s", "10m"))
	defer server.Close()

	store := NewHTTPStore([]string{server.URL}, time.Second, time.Minute)
	cfg, err := store.LoadMetaConfig(context.Background(), MetaConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RefreshInterval != 45*time.Second {
		t.Fatalf("refresh_interval=%v, want 45s", cfg.RefreshInterval)
	}
	if cfg.MaxCacheTTL != 10*time.Minute {
		t.Fatalf("max_cache_ttl=%v, want 10m", cfg.MaxCacheTTL)
	}
}

func TestHTTPStoreRoundRobin(t *testing.T) {
	var callCount1, callCount2 int32
	server1 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount1, 1)
		writeJSON(w, map[string]string{"repo_version": "v1"})
	})
	server2 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount2, 1)
		writeJSON(w, map[string]string{"repo_version": "v1"})
	})
	defer server1.Close()
	defer server2.Close()

	store := NewHTTPStore([]string{server1.URL, server2.URL}, time.Second, time.Minute)
	for i := 0; i < 4; i++ {
		if _, err := store.GetRepoVersion(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if callCount1 != 2 || callCount2 != 2 {
		t.Fatalf("server1 calls=%d, server2 calls=%d, want 2 each", callCount1, callCount2)
	}
}

func TestHTTPStoreFailoverOnServerError(t *testing.T) {
	// server1 总是返回 500
	server1 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	// server2 正常
	var server2Called int32
	server2 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&server2Called, 1)
		writeJSON(w, map[string]string{"repo_version": "v1"})
	})
	defer server1.Close()
	defer server2.Close()

	// 冷却时间设长一些，确保测试期间不会重试 server1
	store := NewHTTPStore([]string{server1.URL, server2.URL}, time.Second, time.Minute)
	version, err := store.GetRepoVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "v1" {
		t.Fatalf("version=%q", version)
	}
	if server2Called != 1 {
		t.Fatalf("server2 calls=%d, want 1", server2Called)
	}
}

func TestHTTPStoreFailoverOnNetworkError(t *testing.T) {
	// server1 关闭（连接被拒绝）
	server1 := newMockServer(func(w http.ResponseWriter, r *http.Request) {})
	server1.Close()
	// server2 正常
	server2 := newMockServer(versionHandler("v2"))
	defer server2.Close()

	store := NewHTTPStore([]string{server1.URL, server2.URL}, time.Second, time.Minute)
	version, err := store.GetRepoVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "v2" {
		t.Fatalf("version=%q, want v2", version)
	}
}

func TestHTTPStoreCooldownSkipsFailedNode(t *testing.T) {
	var server1Calls, server2Calls int32
	server1 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&server1Calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	server2 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&server2Calls, 1)
		writeJSON(w, map[string]string{"repo_version": "v1"})
	})
	defer server1.Close()
	defer server2.Close()

	store := NewHTTPStore([]string{server1.URL, server2.URL}, time.Second, time.Minute)

	// 第一次请求：server1 失败 → 冷却 → 切换到 server2 成功
	if _, err := store.GetRepoVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 第二次请求：server1 在冷却期内被跳过，直接用 server2
	if _, err := store.GetRepoVersion(context.Background()); err != nil {
		t.Fatal(err)
	}

	if server1Calls != 1 {
		t.Fatalf("server1 calls=%d, want 1 (should be skipped in cooldown)", server1Calls)
	}
	if server2Calls != 2 {
		t.Fatalf("server2 calls=%d, want 2", server2Calls)
	}
}

func TestHTTPStoreCooldownExpiryRetriesNode(t *testing.T) {
	var server1Calls int32
	server1 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&server1Calls, 1)
		if count == 1 {
			// 第一次失败
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// 冷却到期后重试，成功
		writeJSON(w, map[string]string{"repo_version": "recovered"})
	})
	server2 := newMockServer(versionHandler("v2"))
	defer server1.Close()
	defer server2.Close()

	// 冷却时间设短一点以便测试
	store := NewHTTPStore([]string{server1.URL, server2.URL}, time.Second, 50*time.Millisecond)

	// 第一次：server1 失败 → server2
	if _, err := store.GetRepoVersion(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&server1Calls) != 1 {
		t.Fatalf("server1 calls after first request=%d", server1Calls)
	}

	// 等待冷却到期
	time.Sleep(100 * time.Millisecond)

	// 第二次：server1 冷却到期，重新尝试 → 成功
	version, err := store.GetRepoVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "recovered" {
		t.Fatalf("version=%q, want recovered", version)
	}
	if atomic.LoadInt32(&server1Calls) != 2 {
		t.Fatalf("server1 calls after retry=%d, want 2", server1Calls)
	}
}

func TestHTTPStoreSuccessClearsFailMemory(t *testing.T) {
	var server1Calls int32
	server1 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&server1Calls, 1)
		if count <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"repo_version": "ok"})
	})
	server2 := newMockServer(versionHandler("v2"))
	defer server1.Close()
	defer server2.Close()

	store := NewHTTPStore([]string{server1.URL, server2.URL}, time.Second, time.Minute)

	// 第一次：server1 失败被标记冷却
	_, _ = store.GetRepoVersion(context.Background())

	// 手动让 server1 冷却到期（通过修改 failUntil）
	store.mu.Lock()
	store.failUntil[server1.URL] = time.Now().Add(-time.Second)
	store.mu.Unlock()

	// 第二次：server1 重试成功，失败记忆被清除
	version, err := store.GetRepoVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "ok" {
		t.Fatalf("version=%q", version)
	}

	// 确认失败记忆已清除
	store.mu.Lock()
	_, failed := store.failUntil[server1.URL]
	store.mu.Unlock()
	if failed {
		t.Fatal("fail memory should have been cleared after success")
	}
}

func TestHTTPStoreAllEndpointsFail(t *testing.T) {
	server1 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	server2 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer server1.Close()
	defer server2.Close()

	store := NewHTTPStore([]string{server1.URL, server2.URL}, time.Second, time.Minute)
	_, err := store.GetRepoVersion(context.Background())
	if err == nil {
		t.Fatal("expected error when all endpoints fail")
	}
}

func TestHTTPStoreAllEndpointsInCooldown(t *testing.T) {
	server1 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	server2 := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer server1.Close()
	defer server2.Close()

	store := NewHTTPStore([]string{server1.URL, server2.URL}, time.Second, time.Minute)
	// 第一次请求标记两个节点都失败
	_, _ = store.GetRepoVersion(context.Background())
	// 第二次请求时两个节点都在冷却期，应直接返回 error
	_, err := store.GetRepoVersion(context.Background())
	if err == nil {
		t.Fatal("expected error when all endpoints are in cooldown")
	}
}

func TestHTTPStoreTimeout(t *testing.T) {
	// server 模拟慢响应
	server := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeJSON(w, map[string]string{"repo_version": "v1"})
	})
	defer server.Close()

	store := NewHTTPStore([]string{server.URL}, 50*time.Millisecond, time.Minute)
	_, err := store.GetRepoVersion(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestHTTPStoreContextCancel(t *testing.T) {
	server := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeJSON(w, map[string]string{"repo_version": "v1"})
	})
	defer server.Close()

	store := NewHTTPStore([]string{server.URL}, time.Second, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := store.GetRepoVersion(ctx)
	if err == nil {
		t.Fatal("expected context deadline error")
	}
}

func TestHTTPStoreEmptyEndpoints(t *testing.T) {
	store := NewHTTPStore(nil, time.Second, time.Minute)
	_, err := store.GetRepoVersion(context.Background())
	if err == nil {
		t.Fatal("expected error with no endpoints")
	}
}

func TestHTTPStoreInvalidJSON(t *testing.T) {
	server := newMockServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{invalid json`)
	})
	defer server.Close()

	store := NewHTTPStore([]string{server.URL}, time.Second, time.Minute)
	_, err := store.GetRepoVersion(context.Background())
	if err == nil {
		t.Fatal("expected JSON parse error")
	}
}
