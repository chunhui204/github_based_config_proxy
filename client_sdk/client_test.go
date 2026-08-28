package client_sdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClientInitLoadsAllConfigs(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: "enabled: true"},
			},
		},
	}
	client := newTestClient(t, store)

	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	value, ok := client.GetConfigOK("payment", "risk.yaml")
	if !ok || value != "enabled: true" {
		t.Fatalf("value=%q ok=%v", value, ok)
	}
}

func TestClientInitReturnsErrorWhenStoreFails(t *testing.T) {
	store := &fakeClientStore{snapshotErr: errors.New("mysql unavailable")}
	client := newTestClient(t, store)

	if err := client.Init(context.Background()); err == nil {
		t.Fatal("expected init error")
	}
}

func TestGetConfigMissing(t *testing.T) {
	client := newTestClient(t, &fakeClientStore{})

	if value, ok := client.GetConfigOK("missing", "app.yaml"); ok || value != "" {
		t.Fatalf("value=%q ok=%v", value, ok)
	}
	if value := client.GetConfig("missing", "app.yaml"); value != "" {
		t.Fatalf("value=%q, want empty", value)
	}
}

func TestRefreshKeepsCacheWhenStoreFails(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: "old"},
			},
		},
		versionErr: errors.New("mysql unavailable"),
	}
	client := newTestClient(t, store)
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	makeVersionCheckExpired(client)

	if err := client.refresh(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	value, ok := client.GetConfigOK("payment", "risk.yaml")
	if !ok || value != "old" {
		t.Fatalf("value=%q ok=%v", value, ok)
	}
}

func TestRefreshOnlyChecksRepoVersionWhenUnchanged(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: "old"},
			},
		},
	}
	client := newTestClient(t, store)
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	makeVersionCheckExpired(client)

	if err := client.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.snapshotCalls != 1 {
		t.Fatalf("snapshot calls=%d, want 1", store.snapshotCalls)
	}
	if store.versionCalls != 1 {
		t.Fatalf("version calls=%d, want 1", store.versionCalls)
	}
}

func TestRefreshReloadsWhenRepoVersionChanged(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: "old"},
			},
		},
		nextVersion: "commit-2",
		nextSnapshot: Snapshot{
			RepoVersion: "commit-2",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: "new"},
			},
		},
	}
	client := newTestClient(t, store)
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	makeVersionCheckExpired(client)

	if err := client.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	value, ok := client.GetConfigOK("payment", "risk.yaml")
	if !ok || value != "new" {
		t.Fatalf("value=%q ok=%v", value, ok)
	}
}

func TestRefreshReplacesSnapshotAndRemovesDeletedConfig(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: "old"},
			},
		},
		nextVersion: "commit-2",
		nextSnapshot: Snapshot{
			RepoVersion: "commit-2",
			Items:       map[ConfigIdentity]ConfigItem{},
		},
	}
	client := newTestClient(t, store)
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	makeVersionCheckExpired(client)

	if err := client.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if value, ok := client.GetConfigOK("payment", "risk.yaml"); ok || value != "" {
		t.Fatalf("value=%q ok=%v", value, ok)
	}
}

type RiskConfig struct {
	Enabled bool  `json:"enabled"`
	Limit   int64 `json:"limit"`
}

func TestRegisterReturnsTypedObjectAfterInit(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.json"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: `{"enabled":true,"limit":100}`},
			},
		},
	}
	client := newTestClient(t, store)

	// 在 Init 之前注册，Init 后应自动反序列化
	riskCfg := Register[RiskConfig](client, "payment", "risk.json")

	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	val, ok := riskCfg.Get()
	if !ok {
		t.Fatal("expected typed config to exist")
	}
	if !val.Enabled || val.Limit != 100 {
		t.Fatalf("unexpected value: %+v", val)
	}
}

func TestRegisterAfterInitDeserializesImmediately(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.json"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: `{"enabled":false,"limit":50}`},
			},
		},
	}
	client := newTestClient(t, store)
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Init 之后注册，应立即反序列化已有字符串
	riskCfg := Register[RiskConfig](client, "payment", "risk.json")
	val, ok := riskCfg.Get()
	if !ok {
		t.Fatal("expected typed config to exist")
	}
	if val.Enabled || val.Limit != 50 {
		t.Fatalf("unexpected value: %+v", val)
	}
}

func TestRefreshUpdatesTypedCacheWhenVersionChanges(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.json"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: `{"enabled":true,"limit":100}`},
			},
		},
		nextVersion: "commit-2",
		nextSnapshot: Snapshot{
			RepoVersion: "commit-2",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: `{"enabled":false,"limit":200}`},
			},
		},
	}
	client := newTestClient(t, store)
	riskCfg := Register[RiskConfig](client, "payment", "risk.json")
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	makeVersionCheckExpired(client)

	if err := client.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	val, ok := riskCfg.Get()
	if !ok {
		t.Fatal("expected typed config to exist")
	}
	if val.Enabled || val.Limit != 200 {
		t.Fatalf("expected updated value, got %+v", val)
	}
}

func TestTypedConfigMissingReturnsFalse(t *testing.T) {
	client := newTestClient(t, &fakeClientStore{})
	riskCfg := Register[RiskConfig](client, "missing", "risk.json")
	if _, ok := riskCfg.Get(); ok {
		t.Fatal("expected missing typed config to return false")
	}
}

func TestRegisterStringSlice(t *testing.T) {
	identity := ConfigIdentity{Namespace: "common", ConfigKey: "whitelist.json"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: `["a","b","c"]`},
			},
		},
	}
	client := newTestClient(t, store)
	whitelist := Register[[]string](client, "common", "whitelist.json")
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	val, ok := whitelist.Get()
	if !ok {
		t.Fatal("expected whitelist to exist")
	}
	if len(val) != 3 || val[0] != "a" || val[1] != "b" || val[2] != "c" {
		t.Fatalf("unexpected slice: %+v", val)
	}
}

type BlacklistItem struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func TestRegisterObjectSlice(t *testing.T) {
	identity := ConfigIdentity{Namespace: "common", ConfigKey: "blacklist.json"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: `[{"id":1,"name":"x"},{"id":2,"name":"y"}]`},
			},
		},
	}
	client := newTestClient(t, store)
	blacklist := Register[[]BlacklistItem](client, "common", "blacklist.json")
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	val, ok := blacklist.Get()
	if !ok {
		t.Fatal("expected blacklist to exist")
	}
	if len(val) != 2 || val[0].ID != 1 || val[0].Name != "x" || val[1].ID != 2 || val[1].Name != "y" {
		t.Fatalf("unexpected object slice: %+v", val)
	}
}

func TestRegisterMapType(t *testing.T) {
	identity := ConfigIdentity{Namespace: "ad", ConfigKey: "channels.json"}
	store := &fakeClientStore{
		snapshot: Snapshot{
			RepoVersion: "commit-1",
			Items: map[ConfigIdentity]ConfigItem{
				identity: {Value: `{"channel_a":{"weight":10},"channel_b":{"weight":20}}`},
			},
		},
	}
	client := newTestClient(t, store)
	type ChannelConfig struct {
		Weight int `json:"weight"`
	}
	channels := Register[map[string]ChannelConfig](client, "ad", "channels.json")
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	val, ok := channels.Get()
	if !ok {
		t.Fatal("expected channels to exist")
	}
	if len(val) != 2 || val["channel_a"].Weight != 10 || val["channel_b"].Weight != 20 {
		t.Fatalf("unexpected map: %+v", val)
	}
}

func newTestClient(t *testing.T, store Store) *Client {
	t.Helper()

	client, err := NewClientWithStore(store, time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func makeVersionCheckExpired(client *Client) {
	client.mu.Lock()
	client.lastVersionCheck = time.Now().Add(-time.Hour)
	client.mu.Unlock()
}

type fakeClientStore struct {
	snapshot      Snapshot
	snapshotErr   error
	nextVersion   string
	versionErr    error
	nextSnapshot  Snapshot
	metaConfig    MetaConfig
	metaConfigErr error
	snapshotCalls int
	versionCalls  int
	metaCalls     int
}

func (s *fakeClientStore) LoadSnapshot(context.Context) (Snapshot, error) {
	if s.snapshotErr != nil {
		return Snapshot{}, s.snapshotErr
	}
	s.snapshotCalls++
	if s.snapshotCalls > 1 && s.nextSnapshot.RepoVersion != "" {
		return cloneSnapshot(s.nextSnapshot), nil
	}
	return cloneSnapshot(s.snapshot), nil
}

func (s *fakeClientStore) GetRepoVersion(context.Context) (string, error) {
	s.versionCalls++
	if s.versionErr != nil {
		return "", s.versionErr
	}
	if s.nextVersion != "" {
		return s.nextVersion, nil
	}
	return s.snapshot.RepoVersion, nil
}

func (s *fakeClientStore) LoadMetaConfig(_ context.Context, cfg MetaConfig) (MetaConfig, error) {
	s.metaCalls++
	if s.metaConfigErr != nil {
		return cfg, s.metaConfigErr
	}
	if s.metaConfig.RefreshInterval > 0 {
		cfg.RefreshInterval = s.metaConfig.RefreshInterval
	}
	if s.metaConfig.MaxCacheTTL > 0 {
		cfg.MaxCacheTTL = s.metaConfig.MaxCacheTTL
	}
	return cfg, nil
}

func cloneSnapshot(source Snapshot) Snapshot {
	items := make(map[ConfigIdentity]ConfigItem, len(source.Items))
	for identity, item := range source.Items {
		items[identity] = item
	}
	return Snapshot{RepoVersion: source.RepoVersion, Items: items}
}

func TestLocalFileStoreLoadSnapshot(t *testing.T) {
	dir := t.TempDir()

	// 模拟 DumpAllConfig 生成的目录结构
	mustMkdir(t, filepath.Join(dir, "payment"))
	mustWrite(t, filepath.Join(dir, "payment", "risk.json"), `{"enabled":true}`)
	mustMkdir(t, filepath.Join(dir, "common"))
	mustWrite(t, filepath.Join(dir, "common", "whitelist.json"), `["a","b"]`)
	mustWrite(t, filepath.Join(dir, repoVersionFileName), "abc123")

	store := NewLocalFileStore(dir)
	ctx := context.Background()

	version, err := store.GetRepoVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != "abc123" {
		t.Fatalf("version=%q, want abc123", version)
	}

	snapshot, err := store.LoadSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RepoVersion != "abc123" {
		t.Fatalf("snapshot version=%q", snapshot.RepoVersion)
	}
	if len(snapshot.Items) != 2 {
		t.Fatalf("items count=%d, want 2", len(snapshot.Items))
	}

	val, ok := snapshot.Items[ConfigIdentity{Namespace: "payment", ConfigKey: "risk.json"}]
	if !ok || val.Value != `{"enabled":true}` {
		t.Fatalf("unexpected item: %+v", val)
	}
}

func TestLocalFileStoreDefaultsVersionWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	store := NewLocalFileStore(dir)

	version, err := store.GetRepoVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "local" {
		t.Fatalf("version=%q, want local", version)
	}
}

func TestLocalFileStoreWorksWithClient(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, filepath.Join(dir, "payment"))
	mustWrite(t, filepath.Join(dir, "payment", "risk.json"), `{"enabled":true,"limit":99}`)

	store := NewLocalFileStore(dir)
	client, err := NewClientWithStore(store, time.Minute, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	riskCfg := Register[RiskConfig](client, "payment", "risk.json")
	if err := client.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	val, ok := riskCfg.Get()
	if !ok || !val.Enabled || val.Limit != 99 {
		t.Fatalf("unexpected typed config: %+v ok=%v", val, ok)
	}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
