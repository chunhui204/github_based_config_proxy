package server

import (
	"context"
	"database/sql"
	"math"
	"os"
	"strings"
	"time"
)

const (
	MetaKeyRepoVersion           = "REPO_VERSION"
	MetaKeyGitHubToken           = "GITHUB_TOKEN"
	MetaKeyGitHubOwner           = "GITHUB_OWNER"
	MetaKeyGitHubRepo            = "GITHUB_REPO"
	MetaKeyGitHubBranch          = "GITHUB_BRANCH"
	MetaKeyGitHubConfigRoot      = "GITHUB_CONFIG_ROOT"
	MetaKeyGitHubBaseURL         = "GITHUB_BASE_URL"
	MetaKeySyncInterval          = "SYNC_INTERVAL"
	MetaKeyLockLeaseTTL          = "LOCK_LEASE_TTL"
	MetaKeyClientRefreshInterval = "CLIENT_REFRESH_INTERVAL"
	MetaKeyClientMaxCacheTTL     = "CLIENT_MAX_CACHE_TTL"
)

type Store interface {
	InitMetadata(ctx context.Context, repo RepoIdentity) error
	TryAcquireLock(ctx context.Context, lockName, ownerID string, leaseTTL time.Duration) (bool, error)
	RenewLock(ctx context.Context, lockName, ownerID string, leaseTTL time.Duration) (bool, error)
	ReleaseLock(ctx context.Context, lockName, ownerID string) error
	GetCheckpoint(ctx context.Context, repo RepoIdentity) (string, error)
	ListCurrent(ctx context.Context) (map[ConfigIdentity]CurrentRecord, error)
	ApplyChanges(ctx context.Context, repo RepoIdentity, commitSHA string, upserts []SyncConfigItem, deletes []DeletedConfigItem) error
}

type MySQLStore struct {
	db *sql.DB
}

func NewMySQLStore(db *sql.DB) *MySQLStore {
	return &MySQLStore{db: db}
}

func (s *MySQLStore) LoadConfigFromMeta(ctx context.Context, cfg Config) (Config, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT META_KEY, META_VALUE
FROM ADS_SERVICE_DYNAMIC_CONFIG_META
WHERE META_KEY IN (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		MetaKeyGitHubToken,
		MetaKeyGitHubOwner,
		MetaKeyGitHubRepo,
		MetaKeyGitHubBranch,
		MetaKeyGitHubConfigRoot,
		MetaKeyGitHubBaseURL,
		MetaKeySyncInterval,
		MetaKeyLockLeaseTTL,
		MetaKeyClientRefreshInterval,
		MetaKeyClientMaxCacheTTL,
	)
	if err != nil {
		return Config{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return Config{}, err
		}
		applyConfigMeta(&cfg, key, value)
	}
	if err := rows.Err(); err != nil {
		return Config{}, err
	}
	cfg.SetDefaults()
	return cfg, nil
}

func (s *MySQLStore) InitMetadata(ctx context.Context, repo RepoIdentity) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT IGNORE INTO ADS_SERVICE_DYNAMIC_CONFIG_SYNC_LEADER_LOCK
    (LOCK_NAME, OWNER_ID, EXPIRE_AT)
VALUES
    (?, '', '1970-01-01 00:00:00')`, DefaultLockName); err != nil {
		return err
	}

	if _, err := s.db.ExecContext(ctx, `
INSERT IGNORE INTO ADS_SERVICE_DYNAMIC_CONFIG_SYNC_CHECKPOINT
    (OWNER, REPO, BRANCH, ROOT_PATH, ROOT_PATH_HASH, LAST_COMMIT_SHA)
VALUES
    (?, ?, ?, ?, ?, '')`, repo.Owner, repo.Repo, repo.Branch, normalizePath(repo.RootPath), RootPathHash(repo.RootPath)); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx, `
INSERT IGNORE INTO ADS_SERVICE_DYNAMIC_CONFIG_META
    (META_KEY, META_VALUE)
VALUES
    (?, '')`, MetaKeyRepoVersion)
	return err
}

func (s *MySQLStore) TryAcquireLock(ctx context.Context, lockName, ownerID string, leaseTTL time.Duration) (bool, error) {
	// 使用 MySQL 时间判断租约，避免不同 server 本地时钟不一致。
	result, err := s.db.ExecContext(ctx, `
UPDATE ADS_SERVICE_DYNAMIC_CONFIG_SYNC_LEADER_LOCK
SET OWNER_ID = ?, EXPIRE_AT = DATE_ADD(NOW(), INTERVAL ? SECOND)
WHERE LOCK_NAME = ?
  AND EXPIRE_AT < NOW()`, ownerID, leaseSeconds(leaseTTL), lockName)
	if err != nil {
		return false, err
	}
	return oneRowAffected(result)
}

func (s *MySQLStore) RenewLock(ctx context.Context, lockName, ownerID string, leaseTTL time.Duration) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE ADS_SERVICE_DYNAMIC_CONFIG_SYNC_LEADER_LOCK
SET EXPIRE_AT = DATE_ADD(NOW(), INTERVAL ? SECOND)
WHERE LOCK_NAME = ?
  AND OWNER_ID = ?`, leaseSeconds(leaseTTL), lockName, ownerID)
	if err != nil {
		return false, err
	}
	return oneRowAffected(result)
}

func (s *MySQLStore) ReleaseLock(ctx context.Context, lockName, ownerID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE ADS_SERVICE_DYNAMIC_CONFIG_SYNC_LEADER_LOCK
SET EXPIRE_AT = NOW()
WHERE LOCK_NAME = ?
  AND OWNER_ID = ?`, lockName, ownerID)
	return err
}

func (s *MySQLStore) GetCheckpoint(ctx context.Context, repo RepoIdentity) (string, error) {
	var commitSHA string
	err := s.db.QueryRowContext(ctx, `
SELECT LAST_COMMIT_SHA
FROM ADS_SERVICE_DYNAMIC_CONFIG_SYNC_CHECKPOINT
WHERE OWNER = ? AND REPO = ? AND BRANCH = ? AND ROOT_PATH_HASH = ? AND ROOT_PATH = ?`,
		repo.Owner, repo.Repo, repo.Branch, RootPathHash(repo.RootPath), normalizePath(repo.RootPath),
	).Scan(&commitSHA)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return commitSHA, err
}

func (s *MySQLStore) ApplyChanges(
	ctx context.Context,
	repo RepoIdentity,
	commitSHA string,
	upserts []SyncConfigItem,
	deletes []DeletedConfigItem,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, item := range upserts {
		item.GitHubCommitSHA = commitSHA
		if err := writeConfigTx(ctx, tx, item, false); err != nil {
			return err
		}
	}
	for _, item := range deletes {
		deletedItem := SyncConfigItem{
			Identity:        item.Identity,
			Path:            item.Path,
			ContentHash:     ContentHash(nil),
			GitHubCommitSHA: commitSHA,
		}
		if err := writeConfigTx(ctx, tx, deletedItem, true); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO ADS_SERVICE_DYNAMIC_CONFIG_SYNC_CHECKPOINT
    (OWNER, REPO, BRANCH, ROOT_PATH, ROOT_PATH_HASH, LAST_COMMIT_SHA, SYNCED_AT)
VALUES
    (?, ?, ?, ?, ?, ?, NOW())
ON DUPLICATE KEY UPDATE
    ROOT_PATH = VALUES(ROOT_PATH),
    LAST_COMMIT_SHA = VALUES(LAST_COMMIT_SHA),
    SYNCED_AT = NOW()`,
		repo.Owner, repo.Repo, repo.Branch, normalizePath(repo.RootPath), RootPathHash(repo.RootPath), commitSHA,
	); err != nil {
		return err
	}

	// client 只读这一行判断仓库整体版本是否变化，避免定时扫描全部配置。
	if _, err := tx.ExecContext(ctx, `
INSERT INTO ADS_SERVICE_DYNAMIC_CONFIG_META
    (META_KEY, META_VALUE)
VALUES
    (?, ?)
ON DUPLICATE KEY UPDATE
    META_VALUE = VALUES(META_VALUE)`, MetaKeyRepoVersion, commitSHA); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MySQLStore) ListCurrent(ctx context.Context) (map[ConfigIdentity]CurrentRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT NAMESPACE, CONFIG_KEY, PATH, CONTENT_HASH, DELETED, GITHUB_COMMIT_SHA
FROM ADS_SERVICE_DYNAMIC_CONFIG_CURRENT`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := make(map[ConfigIdentity]CurrentRecord)
	for rows.Next() {
		var record CurrentRecord
		if err := rows.Scan(
			&record.Identity.Namespace,
			&record.Identity.ConfigKey,
			&record.Path,
			&record.ContentHash,
			&record.Deleted,
			&record.GitHubCommitSHA,
		); err != nil {
			return nil, err
		}
		records[record.Identity] = record
	}
	return records, rows.Err()
}

func writeConfigTx(
	ctx context.Context,
	tx *sql.Tx,
	item SyncConfigItem,
	deleted bool,
) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO ADS_SERVICE_DYNAMIC_CONFIG_CURRENT
    (NAMESPACE, CONFIG_KEY, PATH, CONTENT, CONTENT_HASH, DELETED, GITHUB_COMMIT_SHA)
VALUES
    (?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
    PATH = VALUES(PATH),
    CONTENT = VALUES(CONTENT),
    CONTENT_HASH = VALUES(CONTENT_HASH),
    DELETED = VALUES(DELETED),
    GITHUB_COMMIT_SHA = VALUES(GITHUB_COMMIT_SHA)`,
		item.Identity.Namespace, item.Identity.ConfigKey, item.Path, item.Content, item.ContentHash, deleted, item.GitHubCommitSHA,
	)
	return err
}

func oneRowAffected(result sql.Result) (bool, error) {
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected == 1, nil
}

func leaseSeconds(ttl time.Duration) int {
	seconds := int(math.Ceil(ttl.Seconds()))
	if seconds < 1 {
		return 1
	}
	return seconds
}

func applyConfigMeta(cfg *Config, key, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	switch key {
	case MetaKeyGitHubToken:
		// 环境变量 GITHUB_TOKEN 优先
		if envToken := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); envToken != "" {
			cfg.GitHubToken = envToken
		} else {
			cfg.GitHubToken = value
		}
	case MetaKeyGitHubOwner:
		cfg.GitHubOwner = value
	case MetaKeyGitHubRepo:
		cfg.GitHubRepo = value
	case MetaKeyGitHubBranch:
		cfg.GitHubBranch = value
	case MetaKeyGitHubConfigRoot:
		cfg.GitHubRootPath = value
	case MetaKeyGitHubBaseURL:
		cfg.GitHubBaseURL = value
	case MetaKeySyncInterval:
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			cfg.SyncInterval = d
		}
	case MetaKeyLockLeaseTTL:
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			cfg.LockLeaseTTL = d
		}
	case MetaKeyClientRefreshInterval:
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			cfg.Client.RefreshInterval = d
		}
	case MetaKeyClientMaxCacheTTL:
		if d, err := time.ParseDuration(value); err == nil && d > 0 {
			cfg.Client.MaxCacheTTL = d
		}
	}
}
