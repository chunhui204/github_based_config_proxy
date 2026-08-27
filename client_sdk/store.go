package client_sdk

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

const (
	metaKeyRepoVersion           = "REPO_VERSION"
	metaKeyClientRefreshInterval = "CLIENT_REFRESH_INTERVAL"
	metaKeyClientMaxCacheTTL     = "CLIENT_MAX_CACHE_TTL"
)

type MetaConfig struct {
	RefreshInterval time.Duration
	MaxCacheTTL     time.Duration
}

type Store interface {
	LoadSnapshot(ctx context.Context) (Snapshot, error)
	GetRepoVersion(ctx context.Context) (string, error)
	LoadMetaConfig(ctx context.Context, cfg MetaConfig) (MetaConfig, error)
}

type MySQLStore struct {
	db *sql.DB
}

func NewMySQLStore(db *sql.DB) *MySQLStore {
	return &MySQLStore{db: db}
}

func (s *MySQLStore) LoadMetaConfig(ctx context.Context, cfg MetaConfig) (MetaConfig, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT META_KEY, META_VALUE
FROM ADS_SERVICE_DYNAMIC_CONFIG_META
WHERE META_KEY IN (?, ?)`, metaKeyClientRefreshInterval, metaKeyClientMaxCacheTTL)
	if err != nil {
		return cfg, err
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return cfg, err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			continue
		}
		switch key {
		case metaKeyClientRefreshInterval:
			cfg.RefreshInterval = d
		case metaKeyClientMaxCacheTTL:
			cfg.MaxCacheTTL = d
		}
	}
	if err := rows.Err(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (s *MySQLStore) LoadSnapshot(ctx context.Context) (Snapshot, error) {
	repoVersion, err := s.GetRepoVersion(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT NAMESPACE, CONFIG_KEY, CONTENT, DELETED
FROM ADS_SERVICE_DYNAMIC_CONFIG_CURRENT
WHERE DELETED = 0`)
	if err != nil {
		return Snapshot{}, err
	}
	defer rows.Close()

	items, err := scanConfigItems(rows)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{RepoVersion: repoVersion, Items: items}, nil
}

func (s *MySQLStore) GetRepoVersion(ctx context.Context) (string, error) {
	var repoVersion string
	err := s.db.QueryRowContext(ctx, `
SELECT META_VALUE
FROM ADS_SERVICE_DYNAMIC_CONFIG_META
WHERE META_KEY = ?
LIMIT 1`, metaKeyRepoVersion).Scan(&repoVersion)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return repoVersion, err
}

func scanConfigItems(rows *sql.Rows) (map[ConfigIdentity]ConfigItem, error) {
	now := time.Now()
	items := make(map[ConfigIdentity]ConfigItem)
	for rows.Next() {
		var identity ConfigIdentity
		var item ConfigItem
		if err := rows.Scan(&identity.Namespace, &identity.ConfigKey, &item.Value, &item.Deleted); err != nil {
			return nil, err
		}
		item.LoadedAt = now
		items[identity] = item
	}
	return items, rows.Err()
}
