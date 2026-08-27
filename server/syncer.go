package server

import (
	"context"
	"errors"
	"time"
)

type Syncer struct {
	cfg      Config
	repo     RepoIdentity
	store    Store
	github   GitHubClient
	lockName string
}

func NewSyncer(cfg Config, store Store, github GitHubClient) (*Syncer, error) {
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("store is required")
	}
	if github == nil {
		github = NewGitHubAPIClient(cfg, nil)
	}
	return &Syncer{
		cfg:      cfg,
		repo:     cfg.RepoIdentity(),
		store:    store,
		github:   github,
		lockName: DefaultLockName,
	}, nil
}

func (s *Syncer) Start(ctx context.Context) error {
	if err := s.store.InitMetadata(ctx, s.repo); err != nil {
		return err
	}
	if err := s.SyncOnce(ctx); err != nil {
		return err
	}

	go func() {
		ticker := time.NewTicker(s.cfg.SyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = s.SyncOnce(ctx)
			}
		}
	}()
	return nil
}

func (s *Syncer) SyncOnce(ctx context.Context) error {
	instanceID := s.cfg.InstanceID()
	locked, err := s.store.TryAcquireLock(ctx, s.lockName, instanceID, s.cfg.LockLeaseTTL)
	if err != nil || !locked {
		return err
	}
	renewCtx, stopRenew := context.WithCancel(ctx)
	defer s.store.ReleaseLock(context.Background(), s.lockName, instanceID)
	defer stopRenew()
	go s.renewLock(renewCtx, instanceID)

	checkpoint, err := s.store.GetCheckpoint(ctx, s.repo)
	if err != nil {
		return err
	}
	commitSHA, err := s.github.GetHeadCommit(ctx)
	if err != nil {
		return err
	}
	if checkpoint == commitSHA {
		return nil
	}
	upserts, deletes, err := s.collectChanges(ctx, commitSHA)
	if err != nil {
		return err
	}
	return s.store.ApplyChanges(ctx, s.repo, commitSHA, upserts, deletes)
}

func (s *Syncer) collectChanges(ctx context.Context, commitSHA string) ([]SyncConfigItem, []DeletedConfigItem, error) {
	files, err := s.github.ListFiles(ctx, commitSHA)
	if err != nil {
		return nil, nil, err
	}
	current, err := s.store.ListCurrent(ctx)
	if err != nil {
		return nil, nil, err
	}

	seen := make(map[ConfigIdentity]struct{}, len(files))
	upserts := make([]SyncConfigItem, 0)
	for _, file := range files {
		identity, ok := IdentityFromGitHubPath(s.repo.RootPath, file.Path)
		if !ok {
			continue
		}
		seen[identity] = struct{}{}
		item, changed, err := s.buildUpsert(ctx, file.Path, identity, commitSHA, current[identity])
		if err != nil {
			return nil, nil, err
		}
		if changed {
			upserts = append(upserts, item)
		}
	}
	return upserts, collectDeleted(s.repo.RootPath, commitSHA, current, seen), nil
}

func (s *Syncer) buildUpsert(
	ctx context.Context,
	filePath string,
	identity ConfigIdentity,
	commitSHA string,
	current CurrentRecord,
) (SyncConfigItem, bool, error) {
	content, err := s.github.GetFileContent(ctx, filePath, commitSHA)
	if err != nil {
		return SyncConfigItem{}, false, err
	}
	contentHash := ContentHash(content)
	if !current.Deleted && current.ContentHash == contentHash {
		return SyncConfigItem{}, false, nil
	}
	return SyncConfigItem{
		Identity:        identity,
		Path:            filePath,
		Content:         string(content),
		ContentHash:     contentHash,
		GitHubCommitSHA: commitSHA,
	}, true, nil
}

func collectDeleted(
	rootPath string,
	commitSHA string,
	current map[ConfigIdentity]CurrentRecord,
	seen map[ConfigIdentity]struct{},
) []DeletedConfigItem {
	deletes := make([]DeletedConfigItem, 0)
	for identity, record := range current {
		if record.Deleted {
			continue
		}
		if _, ok := IdentityFromGitHubPath(rootPath, record.Path); !ok {
			continue
		}
		if _, ok := seen[identity]; ok {
			continue
		}
		deletes = append(deletes, DeletedConfigItem{
			Identity:        identity,
			Path:            record.Path,
			GitHubCommitSHA: commitSHA,
		})
	}
	return deletes
}

func (s *Syncer) renewLock(ctx context.Context, instanceID string) {
	interval := s.cfg.LockLeaseTTL / 3
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := s.store.RenewLock(ctx, s.lockName, instanceID, s.cfg.LockLeaseTTL)
			if err != nil || !ok {
				return
			}
		}
	}
}
