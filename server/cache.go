package server

import (
	"sync"
	"time"
)

// CachedItem 是内存缓存中单个配置项的值。
type CachedItem struct {
	Value   string
	Deleted bool
}

// ConfigCache 持有当前配置快照，提供并发安全的读写。
// 配置读多写少（刷新间隔 10s，QPS 可能很高），使用 sync.RWMutex：
// 整体替换快照时用写锁，读取用读锁，业务高并发读取不互斥。
type ConfigCache struct {
	mu          sync.RWMutex
	items       map[ConfigIdentity]CachedItem
	repoVersion string
	loadedAt    time.Time
}

// NewConfigCache 创建一个空的配置缓存。
func NewConfigCache() *ConfigCache {
	return &ConfigCache{
		items: make(map[ConfigIdentity]CachedItem),
	}
}

// Version 返回当前缓存的仓库版本号（GitHub commit SHA）。
func (c *ConfigCache) Version() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.repoVersion
}

// LoadedAt 返回最近一次成功替换缓存的时间。
func (c *ConfigCache) LoadedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loadedAt
}

// Get 读取单个配置项。
// 返回配置内容和是否存在；已删除或不存在的配置返回 ("", false)。
func (c *ConfigCache) Get(namespace, key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	item, ok := c.items[ConfigIdentity{Namespace: namespace, ConfigKey: key}]
	if !ok || item.Deleted {
		return "", false
	}
	return item.Value, true
}

// Snapshot 返回当前所有配置项的副本，避免外部直接修改内部 map。
func (c *ConfigCache) Snapshot() map[ConfigIdentity]CachedItem {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[ConfigIdentity]CachedItem, len(c.items))
	for identity, item := range c.items {
		result[identity] = item
	}
	return result
}

// Replace 用新的快照整体替换缓存内容。
// 构建新 map 后一次性替换引用，保证读侧不会看到部分更新的中间态。
func (c *ConfigCache) Replace(items map[ConfigIdentity]CachedItem, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := make(map[ConfigIdentity]CachedItem, len(items))
	for identity, item := range items {
		next[identity] = item
	}
	c.items = next
	c.repoVersion = version
	c.loadedAt = time.Now()
}
