package client_sdk

import (
	"database/sql"
	"errors"
	"time"
)

// DBConfig 数据库连接配置，业务自己不管理 *sql.DB 时使用。
type DBConfig struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

type Config struct {
	// DB 业务自己已有的 *sql.DB，优先复用；为 nil 时使用 DBConfig 由 SDK 内部创建
	DB              *sql.DB
	DBConfig        *DBConfig
	RefreshInterval time.Duration
	MaxCacheTTL     time.Duration
}

func (c *Config) SetDefaults() {
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = time.Minute
	}
	if c.MaxCacheTTL <= 0 {
		c.MaxCacheTTL = 5 * time.Minute
	}
	if c.DBConfig != nil {
		if c.DBConfig.MaxOpenConns <= 0 {
			c.DBConfig.MaxOpenConns = 100
		}
		if c.DBConfig.MaxIdleConns <= 0 {
			c.DBConfig.MaxIdleConns = 20
		}
		if c.DBConfig.ConnMaxLifetime <= 0 {
			c.DBConfig.ConnMaxLifetime = 30 * time.Minute
		}
	}
}

func (c Config) Validate() error {
	if c.DB == nil && c.DBConfig == nil {
		return errors.New("either DB or DBConfig is required")
	}
	if c.DBConfig != nil && c.DBConfig.DSN == "" {
		return errors.New("DBConfig.DSN is required")
	}
	if c.RefreshInterval <= 0 {
		return errors.New("refresh interval must be positive")
	}
	if c.MaxCacheTTL <= 0 {
		return errors.New("max cache ttl must be positive")
	}
	return nil
}
