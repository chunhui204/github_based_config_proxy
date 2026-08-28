package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	DefaultGitHubBaseURL = "https://api.github.com"
	DefaultLockName      = "GITHUB_CONFIG_SYNC"
	DefaultHTTPPort      = "8080"
	ModeMySQL            = "mysql"
	ModeLocal            = "local"
)

type Config struct {
	GitHubToken          string
	GitHubOwner          string
	GitHubRepo           string
	GitHubBranch         string
	GitHubRootPath       string
	GitHubBaseURL        string
	SyncInterval         time.Duration
	LockLeaseTTL         time.Duration
	CacheRefreshInterval time.Duration
	Database             DatabaseConfig
	Client               ClientConfig
}

type ClientConfig struct {
	RefreshInterval time.Duration
	MaxCacheTTL     time.Duration
}

type DatabaseConfig struct {
	Type            string
	Host            string
	Port            int
	User            string
	Password        string
	DBName          string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

type FileConfig struct {
	Database struct {
		Type            string `json:"type"`
		Host            string `json:"host"`
		Port            int    `json:"port"`
		User            string `json:"user"`
		Password        string `json:"password"`
		DBName          string `json:"db_name"`
		MaxOpenConns    int    `json:"max_open_conns"`
		MaxIdleConns    int    `json:"max_idle_conns"`
		ConnMaxLifetime string `json:"conn_max_lifetime"`
	} `json:"database"`
	GitHub struct {
		Token    string `json:"token"`
		Owner    string `json:"owner"`
		Repo     string `json:"repo"`
		Branch   string `json:"branch"`
		RootPath string `json:"root_path"`
		BaseURL  string `json:"base_url"`
	} `json:"github"`
	CacheRefreshInterval string `json:"cache_refresh_interval"`
}

// NewLocalConfigFromFile 从配置文件和环境变量加载本地模式配置。
// 本地模式不连 MySQL、不选主，直接从 GitHub 拉取配置到内存。
// 环境变量（GITHUB_TOKEN/GITHUB_OWNER/GITHUB_REPO/GITHUB_BRANCH/GITHUB_CONFIG_ROOT）
// 优先级高于配置文件。
func NewLocalConfigFromFile(filePath string) (Config, error) {
	cfg := Config{}
	if filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil && !os.IsNotExist(err) {
			return Config{}, err
		}
		if err == nil {
			var fileConfig FileConfig
			if err := json.Unmarshal(data, &fileConfig); err != nil {
				return Config{}, err
			}
			cfg.GitHubToken = fileConfig.GitHub.Token
			cfg.GitHubOwner = fileConfig.GitHub.Owner
			cfg.GitHubRepo = fileConfig.GitHub.Repo
			cfg.GitHubBranch = fileConfig.GitHub.Branch
			cfg.GitHubRootPath = fileConfig.GitHub.RootPath
			cfg.GitHubBaseURL = fileConfig.GitHub.BaseURL
			cfg.CacheRefreshInterval = parseDuration(fileConfig.CacheRefreshInterval)
		}
	}

	// 环境变量覆盖配置文件
	if v := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); v != "" {
		cfg.GitHubToken = v
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_OWNER")); v != "" {
		cfg.GitHubOwner = v
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_REPO")); v != "" {
		cfg.GitHubRepo = v
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_BRANCH")); v != "" {
		cfg.GitHubBranch = v
	}
	if v := strings.TrimSpace(os.Getenv("GITHUB_CONFIG_ROOT")); v != "" {
		cfg.GitHubRootPath = v
	}

	cfg.SetDefaults()
	return cfg, nil
}

func NewConfigFromFile(filePath string) (Config, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return Config{}, err
	}

	var fileConfig FileConfig
	if err := json.Unmarshal(data, &fileConfig); err != nil {
		return Config{}, err
	}

	cfg := Config{
		Database: DatabaseConfig{
			Type:            fileConfig.Database.Type,
			Host:            fileConfig.Database.Host,
			Port:            fileConfig.Database.Port,
			User:            fileConfig.Database.User,
			Password:        fileConfig.Database.Password,
			DBName:          fileConfig.Database.DBName,
			MaxOpenConns:    fileConfig.Database.MaxOpenConns,
			MaxIdleConns:    fileConfig.Database.MaxIdleConns,
			ConnMaxLifetime: parseDuration(fileConfig.Database.ConnMaxLifetime),
		},
		CacheRefreshInterval: parseDuration(fileConfig.CacheRefreshInterval),
	}
	cfg.SetDefaults()
	return cfg, nil
}

func (c Config) MySQLDSN() string {
	db := c.Database
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?parseTime=true",
		db.User,
		db.Password,
		db.Host,
		db.Port,
		db.DBName,
	)
}

func (c Config) InstanceID() string {
	return os.Getenv("INSTANCE_NAME")
}

func (c Config) ListenAddr() string {
	if addr := strings.TrimSpace(os.Getenv("LISTEN_ADDR")); addr != "" {
		return normalizeListenAddr(addr)
	}
	if port := strings.TrimSpace(os.Getenv("HTTP_PORT")); port != "" {
		return normalizeListenAddr(port)
	}
	return ":" + DefaultHTTPPort
}

func (c Config) RepoIdentity() RepoIdentity {
	return RepoIdentity{
		Owner:    c.GitHubOwner,
		Repo:     c.GitHubRepo,
		Branch:   c.GitHubBranch,
		RootPath: normalizePath(c.GitHubRootPath),
	}
}

func (c *Config) SetDefaults() {
	if c.GitHubBaseURL == "" {
		c.GitHubBaseURL = DefaultGitHubBaseURL
	}
	if c.GitHubBranch == "" {
		c.GitHubBranch = "main"
	}
	if c.SyncInterval <= 0 {
		c.SyncInterval = time.Minute
	}
	if c.LockLeaseTTL <= 0 {
		c.LockLeaseTTL = 2 * c.SyncInterval
	}
	if c.CacheRefreshInterval <= 0 {
		c.CacheRefreshInterval = DefaultCacheRefreshInterval
	}
	if c.Client.RefreshInterval <= 0 {
		c.Client.RefreshInterval = time.Minute
	}
	if c.Client.MaxCacheTTL <= 0 {
		c.Client.MaxCacheTTL = 5 * time.Minute
	}
	if c.Database.Port == 0 {
		c.Database.Port = 3306
	}
	if c.Database.Type == "" {
		c.Database.Type = "mysql"
	}
	if c.Database.MaxOpenConns <= 0 {
		c.Database.MaxOpenConns = 100
	}
	if c.Database.MaxIdleConns <= 0 {
		c.Database.MaxIdleConns = 20
	}
	if c.Database.ConnMaxLifetime <= 0 {
		c.Database.ConnMaxLifetime = 30 * time.Minute
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.InstanceID()) == "" {
		return errors.New("INSTANCE_NAME env is required (set via -e INSTANCE_NAME=xxx when starting container)")
	}
	if err := c.validateGitHub(); err != nil {
		return err
	}
	if c.SyncInterval <= 0 {
		return errors.New("sync interval must be positive")
	}
	if c.LockLeaseTTL <= 0 {
		return errors.New("lock lease ttl must be positive")
	}
	return nil
}

// ValidateLocal 校验本地模式配置。
// 本地模式不需要 INSTANCE_NAME（不选主）、不需要 MySQL、不需要锁配置。
func (c Config) ValidateLocal() error {
	return c.validateGitHub()
}

func (c Config) validateGitHub() error {
	if strings.TrimSpace(c.GitHubToken) == "" {
		return errors.New("github token is required")
	}
	if strings.TrimSpace(c.GitHubOwner) == "" {
		return errors.New("github owner is required")
	}
	if strings.TrimSpace(c.GitHubRepo) == "" {
		return errors.New("github repo is required")
	}
	if strings.TrimSpace(c.GitHubRootPath) == "" {
		return errors.New("github root path is required")
	}
	return nil
}

func parseDuration(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return duration
}

func normalizeListenAddr(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ":" + DefaultHTTPPort
	}
	if strings.HasPrefix(value, ":") {
		return value
	}
	if strings.Contains(value, ":") {
		return value
	}
	return ":" + value
}
