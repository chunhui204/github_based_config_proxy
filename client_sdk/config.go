package client_sdk

import (
	"database/sql"
	"errors"
	"time"
)

const (
	// DefaultServerCircuitBreakerFailureThreshold 是 Server 模式下连续失败后打开熔断的默认阈值。
	DefaultServerCircuitBreakerFailureThreshold = 3
	// DefaultServerCircuitBreakerOpenDuration 是 Server 模式下熔断打开后的默认跳过请求时长。
	DefaultServerCircuitBreakerOpenDuration = 30 * time.Second
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
	DB       *sql.DB
	DBConfig *DBConfig
	// ServerAddrs Config Server 的 HTTP 地址列表（如 ["http://10.0.0.1:8080", "http://10.0.0.2:8080"]）。
	// 非空时 SDK 通过 HTTP 请求 Server 获取配置，不直连 MySQL；DB/DBConfig 字段被忽略。
	ServerAddrs []string
	// HTTPTimeout 单次 HTTP 请求超时（仅 ServerAddrs 模式生效）。
	HTTPTimeout time.Duration
	// FailBackoff 节点失败后冷却时长（仅 ServerAddrs 模式生效）。
	FailBackoff time.Duration
	// ServerCircuitBreakerFailureThreshold 连续请求 Server 失败多少次后打开 SDK 级熔断。
	ServerCircuitBreakerFailureThreshold int
	// ServerCircuitBreakerOpenDuration 熔断打开后多久内直接使用本地缓存，不再请求 Server。
	ServerCircuitBreakerOpenDuration time.Duration
	RefreshInterval                  time.Duration
	MaxCacheTTL                      time.Duration
}

func (c *Config) SetDefaults() {
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = time.Minute
	}
	if c.MaxCacheTTL <= 0 {
		c.MaxCacheTTL = 5 * time.Minute
	}
	if c.ServerCircuitBreakerFailureThreshold <= 0 {
		c.ServerCircuitBreakerFailureThreshold = DefaultServerCircuitBreakerFailureThreshold
	}
	if c.ServerCircuitBreakerOpenDuration <= 0 {
		c.ServerCircuitBreakerOpenDuration = DefaultServerCircuitBreakerOpenDuration
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
	hasDB := c.DB != nil || c.DBConfig != nil
	hasServer := len(c.ServerAddrs) > 0
	if !hasDB && !hasServer {
		return errors.New("either DB/DBConfig or ServerAddrs is required")
	}
	if c.DBConfig != nil && c.DBConfig.DSN == "" {
		return errors.New("DBConfig.DSN is required")
	}
	if hasServer {
		for _, addr := range c.ServerAddrs {
			if addr == "" {
				return errors.New("ServerAddrs contains empty address")
			}
		}
		if c.ServerCircuitBreakerFailureThreshold <= 0 {
			return errors.New("server circuit breaker failure threshold must be positive")
		}
		if c.ServerCircuitBreakerOpenDuration <= 0 {
			return errors.New("server circuit breaker open duration must be positive")
		}
	}
	if c.RefreshInterval <= 0 {
		return errors.New("refresh interval must be positive")
	}
	if c.MaxCacheTTL <= 0 {
		return errors.New("max cache ttl must be positive")
	}
	return nil
}
