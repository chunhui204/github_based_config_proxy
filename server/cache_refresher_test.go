package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSnapshotReader 是 SnapshotReader 的测试替身。
// 计数器使用 atomic 以支持后台刷新 goroutine 并发访问。
type fakeSnapshotReader struct {
	version      string
	versionErr   error
	snapshot     CacheSnapshot
	snapshotErr  error
	versionCalls int64
	snapshotCalls int64
}

func (r *fakeSnapshotReader) GetRepoVersion(context.Context) (string, error) {
	atomic.AddInt64(&r.versionCalls, 1)
	return r.version, r.versionErr
}

func (r *fakeSnapshotReader) LoadSnapshot(context.Context) (CacheSnapshot, error) {
	atomic.AddInt64(&r.snapshotCalls, 1)
	return r.snapshot, r.snapshotErr
}

func TestCacheRefresherStartLoadsInitialSnapshot(t *testing.T) {
	reader := &fakeSnapshotReader{
		version: "v1",
		snapshot: CacheSnapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]CachedItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "a"},
			},
		},
	}
	cache := NewConfigCache()
	refresher := NewCacheRefresher(reader, cache, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := refresher.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if cache.Version() != "v1" {
		t.Fatalf("version=%q, want v1", cache.Version())
	}
	value, ok := cache.Get("ns", "a.yaml")
	if !ok || value != "a" {
		t.Fatalf("value=%q ok=%v", value, ok)
	}
}

func TestCacheRefresherStartReturnsErrorOnInitialFailure(t *testing.T) {
	reader := &fakeSnapshotReader{versionErr: errors.New("source unavailable")}
	cache := NewConfigCache()
	refresher := NewCacheRefresher(reader, cache, time.Hour)

	if err := refresher.Start(context.Background()); err == nil {
		t.Fatal("expected error on initial load failure")
	}
}

func TestCacheRefresherSkipsLoadWhenVersionUnchanged(t *testing.T) {
	reader := &fakeSnapshotReader{
		version: "v1",
		snapshot: CacheSnapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]CachedItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "a"},
			},
		},
	}
	cache := NewConfigCache()
	refresher := NewCacheRefresher(reader, cache, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := refresher.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// 手动触发第二次刷新，版本不变
	atomic.StoreInt64(&reader.versionCalls, 0)
	atomic.StoreInt64(&reader.snapshotCalls, 0)
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := atomic.LoadInt64(&reader.snapshotCalls); calls != 0 {
		t.Fatalf("snapshot calls=%d, want 0 (version unchanged)", calls)
	}
}

func TestCacheRefresherReloadsWhenVersionChanged(t *testing.T) {
	reader := &fakeSnapshotReader{
		version: "v1",
		snapshot: CacheSnapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]CachedItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "old"},
			},
		},
	}
	cache := NewConfigCache()
	refresher := NewCacheRefresher(reader, cache, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := refresher.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// 版本变化
	reader.version = "v2"
	reader.snapshot = CacheSnapshot{
		RepoVersion: "v2",
		Items: map[ConfigIdentity]CachedItem{
			{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "new"},
		},
	}
	if err := refresher.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if cache.Version() != "v2" {
		t.Fatalf("version=%q, want v2", cache.Version())
	}
	value, _ := cache.Get("ns", "a.yaml")
	if value != "new" {
		t.Fatalf("value=%q, want new", value)
	}
}

func TestCacheRefresherKeepsOldCacheOnFailure(t *testing.T) {
	reader := &fakeSnapshotReader{
		version: "v1",
		snapshot: CacheSnapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]CachedItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "stable"},
			},
		},
	}
	cache := NewConfigCache()
	refresher := NewCacheRefresher(reader, cache, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := refresher.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// 版本变了但加载失败
	reader.version = "v2"
	reader.snapshotErr = errors.New("network error")
	if err := refresher.refresh(ctx); err == nil {
		t.Fatal("expected error")
	}

	// 旧缓存必须保留
	if cache.Version() != "v1" {
		t.Fatalf("version=%q, want v1 (old cache preserved)", cache.Version())
	}
	value, ok := cache.Get("ns", "a.yaml")
	if !ok || value != "stable" {
		t.Fatalf("value=%q ok=%v, want stable/true", value, ok)
	}
}

func TestCacheRefresherVersionErrorKeepsOldCache(t *testing.T) {
	reader := &fakeSnapshotReader{
		version: "v1",
		snapshot: CacheSnapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]CachedItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "stable"},
			},
		},
	}
	cache := NewConfigCache()
	refresher := NewCacheRefresher(reader, cache, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := refresher.Start(ctx); err != nil {
		t.Fatal(err)
	}

	reader.versionErr = errors.New("version check failed")
	if err := refresher.refresh(ctx); err == nil {
		t.Fatal("expected error")
	}
	if cache.Version() != "v1" {
		t.Fatalf("version=%q, want v1", cache.Version())
	}
}

func TestCacheRefresherPeriodicRefresh(t *testing.T) {
	reader := &fakeSnapshotReader{
		version: "v1",
		snapshot: CacheSnapshot{
			RepoVersion: "v1",
			Items: map[ConfigIdentity]CachedItem{
				{Namespace: "ns", ConfigKey: "a.yaml"}: {Value: "v1"},
			},
		},
	}
	cache := NewConfigCache()
	// 使用很短的间隔测试后台刷新
	refresher := NewCacheRefresher(reader, cache, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := refresher.Start(ctx); err != nil {
		t.Fatal(err)
	}

	// 等待至少两次后台刷新
	time.Sleep(100 * time.Millisecond)
	if calls := atomic.LoadInt64(&reader.versionCalls); calls < 2 {
		t.Fatalf("version calls=%d, expected at least 2 periodic refreshes", calls)
	}
}
