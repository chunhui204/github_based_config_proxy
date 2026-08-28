package server

import (
	"encoding/json"
	"net/http"
	"time"
)

// MetaResponse 是 /api/v1/meta 接口的响应体。
type MetaResponse struct {
	RefreshInterval string `json:"refresh_interval"`
	MaxCacheTTL     string `json:"max_cache_ttl"`
}

// versionResponse 是 /api/v1/version 接口的响应体。
type versionResponse struct {
	RepoVersion string `json:"repo_version"`
}

// configItemDTO 是 /api/v1/snapshot 响应中单个配置项的传输结构。
type configItemDTO struct {
	Namespace string `json:"namespace"`
	ConfigKey string `json:"config_key"`
	Value     string `json:"value"`
	Deleted   bool   `json:"deleted"`
}

// snapshotResponse 是 /api/v1/snapshot 接口的响应体。
type snapshotResponse struct {
	RepoVersion string          `json:"repo_version"`
	Items       []configItemDTO `json:"items"`
}

// NewConfigHandler 创建配置查询 HTTP Handler。
// 所有接口直接从 ConfigCache 读取，不访问 MySQL/GitHub。
// clientConfig 用于 /api/v1/meta 返回 Client 侧的刷新间隔配置。
func NewConfigHandler(cache *ConfigCache, clientConfig ClientConfig) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, versionResponse{RepoVersion: cache.Version()})
	})

	mux.HandleFunc("/api/v1/meta", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, MetaResponse{
			RefreshInterval: durationString(clientConfig.RefreshInterval),
			MaxCacheTTL:     durationString(clientConfig.MaxCacheTTL),
		})
	})

	mux.HandleFunc("/api/v1/config", func(w http.ResponseWriter, r *http.Request) {
		namespace := r.URL.Query().Get("namespace")
		key := r.URL.Query().Get("key")
		if namespace == "" || key == "" {
			http.Error(w, "namespace and key are required", http.StatusBadRequest)
			return
		}
		value, ok := cache.Get(namespace, key)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(value))
	})

	mux.HandleFunc("/api/v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		snapshot := cache.Snapshot()
		items := make([]configItemDTO, 0, len(snapshot))
		for identity, item := range snapshot {
			if item.Deleted {
				continue
			}
			items = append(items, configItemDTO{
				Namespace: identity.Namespace,
				ConfigKey: identity.ConfigKey,
				Value:     item.Value,
				Deleted:   item.Deleted,
			})
		}
		writeJSON(w, snapshotResponse{
			RepoVersion: cache.Version(),
			Items:       items,
		})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func durationString(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}
