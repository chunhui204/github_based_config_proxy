package server

import "context"

// SnapshotReader 是配置快照的只读数据源抽象。
// Server 的缓存刷新逻辑只依赖该接口，不关心数据来自 MySQL 还是 GitHub，
// 从而让 MySQL 模式和本地模式共用同一套刷新逻辑。
type SnapshotReader interface {
	// GetRepoVersion 返回当前仓库版本号（GitHub commit SHA）。
	GetRepoVersion(ctx context.Context) (string, error)
	// LoadSnapshot 全量加载当前所有未删除的配置项。
	LoadSnapshot(ctx context.Context) (CacheSnapshot, error)
}

// CacheSnapshot 是一次全量加载的配置快照。
type CacheSnapshot struct {
	RepoVersion string
	Items       map[ConfigIdentity]CachedItem
}
