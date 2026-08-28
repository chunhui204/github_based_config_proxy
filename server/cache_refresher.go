package server

import (
	"context"
	"log"
	"time"
)

// DefaultCacheRefreshInterval 是 Server 从数据源（MySQL/GitHub）刷新本地缓存的默认间隔。
const DefaultCacheRefreshInterval = 10 * time.Second

// CacheRefresher 定期从 SnapshotReader 加载配置快照并替换 ConfigCache。
// 刷新逻辑与 Client SDK 的 refresh 对称：先比版本，版本变了才全量加载，
// 任何一步失败都保留旧缓存，等待下一轮重试。
type CacheRefresher struct {
	reader   SnapshotReader
	cache    *ConfigCache
	interval time.Duration
}

// NewCacheRefresher 创建缓存刷新器。
// interval <= 0 时使用 DefaultCacheRefreshInterval。
func NewCacheRefresher(reader SnapshotReader, cache *ConfigCache, interval time.Duration) *CacheRefresher {
	if interval <= 0 {
		interval = DefaultCacheRefreshInterval
	}
	return &CacheRefresher{
		reader:   reader,
		cache:    cache,
		interval: interval,
	}
}

// Start 执行首次同步加载（失败则返回 error 阻止启动），然后启动后台定期刷新 goroutine。
func (r *CacheRefresher) Start(ctx context.Context) error {
	if err := r.refresh(ctx); err != nil {
		return err
	}

	go func() {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.refresh(ctx); err != nil {
					log.Printf("cache refresh failed: %v", err)
				}
			}
		}
	}()
	return nil
}

// refresh 执行一次刷新：先查版本，版本不变则跳过；变化则全量加载并替换缓存。
func (r *CacheRefresher) refresh(ctx context.Context) error {
	remoteVersion, err := r.reader.GetRepoVersion(ctx)
	if err != nil {
		return err
	}
	if remoteVersion == r.cache.Version() {
		return nil
	}

	snapshot, err := r.reader.LoadSnapshot(ctx)
	if err != nil {
		return err
	}
	r.cache.Replace(snapshot.Items, snapshot.RepoVersion)
	log.Printf("cache refreshed: version=%s items=%d", snapshot.RepoVersion, len(snapshot.Items))
	return nil
}
