package client_sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	// DefaultHTTPTimeout 是单次 HTTP 请求的默认超时。
	DefaultHTTPTimeout = 3 * time.Second
	// DefaultFailBackoff 是节点失败后的冷却时长。
	DefaultFailBackoff = 30 * time.Second
)

// HTTPStore 通过 HTTP 请求 Config Server 获取配置，实现 Store 接口。
// 内置多 endpoint 轮询负载均衡和短期失败记忆（冷却），单点故障时自动切换。
type HTTPStore struct {
	endpoints   []string
	httpClient  *http.Client
	failBackoff time.Duration

	mu        sync.Mutex
	current   int
	failUntil map[string]time.Time
}

// NewHTTPStore 创建一个从 Config Server 获取配置的 Store。
// timeout <= 0 时使用 DefaultHTTPTimeout；backoff <= 0 时使用 DefaultFailBackoff。
func NewHTTPStore(endpoints []string, timeout, backoff time.Duration) *HTTPStore {
	if timeout <= 0 {
		timeout = DefaultHTTPTimeout
	}
	if backoff <= 0 {
		backoff = DefaultFailBackoff
	}
	return &HTTPStore{
		endpoints:   append([]string(nil), endpoints...),
		httpClient:  &http.Client{Timeout: timeout},
		failBackoff: backoff,
		failUntil:   make(map[string]time.Time),
	}
}

// GetRepoVersion 查询 Server 的仓库版本号。
func (s *HTTPStore) GetRepoVersion(ctx context.Context) (string, error) {
	var resp struct {
		RepoVersion string `json:"repo_version"`
	}
	if err := s.doRequest(ctx, "/api/v1/version", &resp); err != nil {
		return "", err
	}
	return resp.RepoVersion, nil
}

// LoadSnapshot 全量加载配置快照。
func (s *HTTPStore) LoadSnapshot(ctx context.Context) (Snapshot, error) {
	var resp struct {
		RepoVersion string `json:"repo_version"`
		Items       []struct {
			Namespace string `json:"namespace"`
			ConfigKey string `json:"config_key"`
			Value     string `json:"value"`
			Deleted   bool   `json:"deleted"`
		} `json:"items"`
	}
	if err := s.doRequest(ctx, "/api/v1/snapshot", &resp); err != nil {
		return Snapshot{}, err
	}

	items := make(map[ConfigIdentity]ConfigItem, len(resp.Items))
	for _, item := range resp.Items {
		items[ConfigIdentity{Namespace: item.Namespace, ConfigKey: item.ConfigKey}] = ConfigItem{
			Value:    item.Value,
			Deleted:  item.Deleted,
			LoadedAt: time.Now(),
		}
	}
	return Snapshot{RepoVersion: resp.RepoVersion, Items: items}, nil
}

// LoadMetaConfig 查询 Server 的 Client 侧刷新间隔配置。
func (s *HTTPStore) LoadMetaConfig(ctx context.Context, cfg MetaConfig) (MetaConfig, error) {
	var resp struct {
		RefreshInterval string `json:"refresh_interval"`
		MaxCacheTTL     string `json:"max_cache_ttl"`
	}
	if err := s.doRequest(ctx, "/api/v1/meta", &resp); err != nil {
		return cfg, err
	}
	if d, err := time.ParseDuration(resp.RefreshInterval); err == nil && d > 0 {
		cfg.RefreshInterval = d
	}
	if d, err := time.ParseDuration(resp.MaxCacheTTL); err == nil && d > 0 {
		cfg.MaxCacheTTL = d
	}
	return cfg, nil
}

// doRequest 在多个 endpoint 之间轮询，跳过冷却期内的节点，失败时自动切换。
// path 必须以 / 开头，如 /api/v1/version。
func (s *HTTPStore) doRequest(ctx context.Context, path string, out any) error {
	s.mu.Lock()
	if len(s.endpoints) == 0 {
		s.mu.Unlock()
		return fmt.Errorf("no server endpoints configured")
	}

	now := time.Now()
	// 从当前游标开始，遍历所有节点，跳过冷却期内的
	var lastErr error
	for i := 0; i < len(s.endpoints); i++ {
		idx := (s.current + i) % len(s.endpoints)
		endpoint := s.endpoints[idx]

		if until, ok := s.failUntil[endpoint]; ok && now.Before(until) {
			continue
		}

		// 推进游标到下一个节点，下次请求从下一个开始
		s.current = (idx + 1) % len(s.endpoints)
		s.mu.Unlock()

		err := s.fetchJSON(ctx, endpoint+path, out)
		if err == nil {
			// 请求成功，清除该节点的失败记忆
			s.markSuccess(endpoint)
			return nil
		}
		lastErr = err
		s.markFailed(endpoint)

		// 重新加锁，继续尝试下一个节点
		s.mu.Lock()
		now = time.Now()
	}
	s.mu.Unlock()

	if lastErr != nil {
		return fmt.Errorf("all server endpoints failed, last error: %w", lastErr)
	}
	return fmt.Errorf("all server endpoints are in cooldown")
}

// fetchJSON 发起单次 HTTP GET 请求并解析 JSON 响应。
func (s *HTTPStore) fetchJSON(ctx context.Context, fullURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// markFailed 记录节点失败，设置冷却到期时间。
func (s *HTTPStore) markFailed(endpoint string) {
	s.mu.Lock()
	s.failUntil[endpoint] = time.Now().Add(s.failBackoff)
	s.mu.Unlock()
}

// markSuccess 清除节点的失败记忆。
func (s *HTTPStore) markSuccess(endpoint string) {
	s.mu.Lock()
	delete(s.failUntil, endpoint)
	s.mu.Unlock()
}
