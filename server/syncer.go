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
	configs, err := ListGitHubConfigs(ctx, s.github, s.repo.RootPath, commitSHA)
	if err != nil {
		return nil, nil, err
	}
	current, err := s.store.ListCurrent(ctx)
	if err != nil {
		return nil, nil, err
	}

	upserts := make([]SyncConfigItem, 0, len(configs))
	for identity, cfg := range configs {
		contentHash := ContentHash(cfg.Content)
		record := current[identity]
		if !record.Deleted && record.ContentHash == contentHash {
			continue
		}
		upserts = append(upserts, SyncConfigItem{
			Identity:        identity,
			Path:            cfg.Path,
			Content:         string(cfg.Content),
			ContentHash:     contentHash,
			GitHubCommitSHA: commitSHA,
		})
	}
	return upserts, collectDeleted(s.repo.RootPath, commitSHA, current, configs), nil
}

func collectDeleted(
	rootPath string,
	commitSHA string,
	current map[ConfigIdentity]CurrentRecord,
	configs map[ConfigIdentity]FetchedConfig,
) []DeletedConfigItem {
	deletes := make([]DeletedConfigItem, 0)
	for identity, record := range current {
		if record.Deleted {
			continue
		}
		if _, ok := IdentityFromGitHubPath(rootPath, record.Path); !ok {
			continue
		}
		if _, ok := configs[identity]; ok {
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
