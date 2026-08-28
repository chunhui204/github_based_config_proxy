package server

import (
	"sync"
	"testing"
)

func TestConfigCacheGetMissing(t *testing.T) {
	cache := NewConfigCache()
	if value, ok := cache.Get("ns", "missing.yaml"); ok || value != "" {
		t.Fatalf("value=%q ok=%v, want empty/false", value, ok)
	}
}

func TestConfigCacheReplaceAndGet(t *testing.T) {
	cache := NewConfigCache()
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}
	items := map[ConfigIdentity]CachedItem{
		identity: {Value: "enabled: true"},
	}
	cache.Replace(items, "commit-abc")

	value, ok := cache.Get("payment", "risk.yaml")
	if !ok || value != "enabled: true" {
		t.Fatalf("value=%q ok=%v", value, ok)
	}
	if v := cache.Version(); v != "commit-abc" {
		t.Fatalf("version=%q, want commit-abc", v)
	}
}

func TestConfigCacheDeletedItemReturnsFalse(t *testing.T) {
	cache := NewConfigCache()
	identity := ConfigIdentity{Namespace: "ns", ConfigKey: "deleted.yaml"}
	cache.Replace(map[ConfigIdentity]CachedItem{
		identity: {Value: "", Deleted: true},
	}, "v1")

	if _, ok := cache.Get("ns", "deleted.yaml"); ok {
		t.Fatal("expected deleted item to return false")
	}
}

func TestConfigCacheSnapshotReturnsCopy(t *testing.T) {
	cache := NewConfigCache()
	identity := ConfigIdentity{Namespace: "ns", ConfigKey: "a.yaml"}
	cache.Replace(map[ConfigIdentity]CachedItem{
		identity: {Value: "v1"},
	}, "v1")

	// 修改返回的副本不应影响内部状态
	snap := cache.Snapshot()
	snap[identity] = CachedItem{Value: "tampered"}

	value, _ := cache.Get("ns", "a.yaml")
	if value != "v1" {
		t.Fatalf("internal cache mutated: value=%q", value)
	}
}

func TestConfigCacheReplaceOverwritesOldItems(t *testing.T) {
	cache := NewConfigCache()
	cache.Replace(map[ConfigIdentity]CachedItem{
		{Namespace: "ns", ConfigKey: "old.yaml"}: {Value: "old"},
	}, "v1")

	// 新快照不包含 old.yaml，替换后应不可见
	cache.Replace(map[ConfigIdentity]CachedItem{
		{Namespace: "ns", ConfigKey: "new.yaml"}: {Value: "new"},
	}, "v2")

	if _, ok := cache.Get("ns", "old.yaml"); ok {
		t.Fatal("old item should have been removed after replace")
	}
	value, ok := cache.Get("ns", "new.yaml")
	if !ok || value != "new" {
		t.Fatalf("new item value=%q ok=%v", value, ok)
	}
}

func TestConfigCacheConcurrentReadWrite(t *testing.T) {
	cache := NewConfigCache()
	cache.Replace(map[ConfigIdentity]CachedItem{
		{Namespace: "ns", ConfigKey: "k.yaml"}: {Value: "v0"},
	}, "v0")

	var readersWg sync.WaitGroup
	var writersWg sync.WaitGroup
	stop := make(chan struct{})

	// 并发读
	for i := 0; i < 10; i++ {
		readersWg.Add(1)
		go func() {
			defer readersWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = cache.Get("ns", "k.yaml")
					_ = cache.Version()
					_ = cache.Snapshot()
				}
			}
		}()
	}

	// 并发写
	for i := 0; i < 5; i++ {
		writersWg.Add(1)
		go func() {
			defer writersWg.Done()
			for j := 0; j < 100; j++ {
				cache.Replace(map[ConfigIdentity]CachedItem{
					{Namespace: "ns", ConfigKey: "k.yaml"}: {Value: "v"},
				}, "v")
			}
		}()
	}

	// 等写完成后停止读
	writersWg.Wait()
	close(stop)
	readersWg.Wait()
}
