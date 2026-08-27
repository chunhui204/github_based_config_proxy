package server

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSyncOnceSkipsWhenLockUnavailable(t *testing.T) {
	store := newFakeStore(false)
	github := &fakeGitHub{head: "commit-1"}
	syncer := newTestSyncer(t, store, github)

	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if github.headCalls != 0 {
		t.Fatalf("head calls=%d, want 0", github.headCalls)
	}
}

func TestSyncOnceSkipsWhenCheckpointUnchanged(t *testing.T) {
	store := newFakeStore(true)
	store.checkpoint = "commit-1"
	github := &fakeGitHub{head: "commit-1"}
	syncer := newTestSyncer(t, store, github)

	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if github.listCalls != 0 {
		t.Fatalf("list calls=%d, want 0", github.listCalls)
	}
}

func TestSyncOnceUpsertsNewConfig(t *testing.T) {
	store := newFakeStore(true)
	github := &fakeGitHub{
		head:  "commit-1",
		files: []GitHubFile{{Path: "configs/payment/risk.yaml"}},
		content: map[string][]byte{
			"configs/payment/risk.yaml": []byte("enabled: true"),
		},
	}
	syncer := newTestSyncer(t, store, github)

	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.upserts) != 1 {
		t.Fatalf("upsert count=%d, want 1", len(store.upserts))
	}
	got := store.upserts[0]
	if got.Identity != (ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}) {
		t.Fatalf("identity=%+v", got.Identity)
	}
	if got.ContentHash != ContentHash([]byte("enabled: true")) {
		t.Fatalf("content hash=%s", got.ContentHash)
	}
	if store.checkpoint != "commit-1" {
		t.Fatalf("checkpoint=%s, want commit-1", store.checkpoint)
	}
}

func TestSyncOnceSkipsUnchangedContent(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}
	content := []byte("enabled: true")
	store := newFakeStore(true)
	store.current[identity] = CurrentRecord{
		Identity:    identity,
		Path:        "configs/payment/risk.yaml",
		ContentHash: ContentHash(content),
	}
	github := &fakeGitHub{
		head:  "commit-2",
		files: []GitHubFile{{Path: "configs/payment/risk.yaml"}},
		content: map[string][]byte{
			"configs/payment/risk.yaml": content,
		},
	}
	syncer := newTestSyncer(t, store, github)

	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.upserts) != 0 {
		t.Fatalf("upsert count=%d, want 0", len(store.upserts))
	}
}

func TestSyncOnceMarksDeletedConfig(t *testing.T) {
	identity := ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}
	store := newFakeStore(true)
	store.current[identity] = CurrentRecord{
		Identity:    identity,
		Path:        "configs/payment/risk.yaml",
		ContentHash: ContentHash([]byte("enabled: true")),
	}
	github := &fakeGitHub{head: "commit-2"}
	syncer := newTestSyncer(t, store, github)

	if err := syncer.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.deletes) != 1 {
		t.Fatalf("delete count=%d, want 1", len(store.deletes))
	}
	if store.deletes[0].Identity != identity {
		t.Fatalf("deleted identity=%+v", store.deletes[0].Identity)
	}
}

func newTestSyncer(t *testing.T, store *fakeStore, github *fakeGitHub) *Syncer {
	t.Helper()

	oldInstanceName := os.Getenv("INSTANCE_NAME")
	os.Setenv("INSTANCE_NAME", "test-instance")
	t.Cleanup(func() {
		os.Setenv("INSTANCE_NAME", oldInstanceName)
	})

	cfg := Config{
		GitHubToken:    "token",
		GitHubOwner:    "owner",
		GitHubRepo:     "repo",
		GitHubBranch:   "main",
		GitHubRootPath: "configs",
		SyncInterval:   time.Hour,
		LockLeaseTTL:   time.Hour,
	}
	syncer, err := NewSyncer(cfg, store, github)
	if err != nil {
		t.Fatal(err)
	}
	return syncer
}

type fakeGitHub struct {
	head      string
	files     []GitHubFile
	content   map[string][]byte
	headCalls int
	listCalls int
}

func (g *fakeGitHub) GetHeadCommit(context.Context) (string, error) {
	g.headCalls++
	return g.head, nil
}

func (g *fakeGitHub) ListFiles(context.Context, string) ([]GitHubFile, error) {
	g.listCalls++
	return g.files, nil
}

func (g *fakeGitHub) GetFileContent(_ context.Context, filePath string, _ string) ([]byte, error) {
	return g.content[filePath], nil
}

type fakeStore struct {
	lockable   bool
	checkpoint string
	current    map[ConfigIdentity]CurrentRecord
	upserts    []SyncConfigItem
	deletes    []DeletedConfigItem
}

func newFakeStore(lockable bool) *fakeStore {
	return &fakeStore{
		lockable: lockable,
		current:  make(map[ConfigIdentity]CurrentRecord),
	}
}

func (s *fakeStore) InitMetadata(context.Context, RepoIdentity) error {
	return nil
}

func (s *fakeStore) TryAcquireLock(context.Context, string, string, time.Duration) (bool, error) {
	return s.lockable, nil
}

func (s *fakeStore) RenewLock(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (s *fakeStore) ReleaseLock(context.Context, string, string) error {
	return nil
}

func (s *fakeStore) GetCheckpoint(context.Context, RepoIdentity) (string, error) {
	return s.checkpoint, nil
}

func (s *fakeStore) ApplyChanges(
	_ context.Context,
	_ RepoIdentity,
	commitSHA string,
	upserts []SyncConfigItem,
	deletes []DeletedConfigItem,
) error {
	s.checkpoint = commitSHA
	s.upserts = append(s.upserts, upserts...)
	s.deletes = append(s.deletes, deletes...)
	for _, item := range upserts {
		s.current[item.Identity] = CurrentRecord{
			Identity:        item.Identity,
			Path:            item.Path,
			ContentHash:     item.ContentHash,
			GitHubCommitSHA: item.GitHubCommitSHA,
		}
	}
	for _, item := range deletes {
		record := s.current[item.Identity]
		record.Deleted = true
		s.current[item.Identity] = record
	}
	return nil
}

func (s *fakeStore) ListCurrent(context.Context) (map[ConfigIdentity]CurrentRecord, error) {
	records := make(map[ConfigIdentity]CurrentRecord, len(s.current))
	for identity, record := range s.current {
		records[identity] = record
	}
	return records, nil
}
