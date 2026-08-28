package client_sdk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

type unmarshalFunc func(value string) (any, error)

type Client struct {
	cfg   Config
	store Store
	// closeDB 内部创建 db 时的关闭函数；业务传入 db 时为 nil，不关闭
	closeDB func() error

	mu               sync.RWMutex
	cache            map[ConfigIdentity]ConfigItem
	typedRegs        map[ConfigIdentity]unmarshalFunc
	typedCache       map[ConfigIdentity]any
	repoVersion      string
	initialized      bool
	lastVersionCheck time.Time
	lastRefreshErr   error
	cancel           context.CancelFunc
}

// NewClient 创建配置客户端。
// 如果 cfg.ServerAddrs 非空，通过 HTTP 请求 Config Server 获取配置（不直连 MySQL）；
// 否则使用 cfg.DB 或 cfg.DBConfig 直连 MySQL。
func NewClient(cfg Config) (*Client, error) {
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// ServerAddrs 模式：通过 HTTP 请求 Config Server
	if len(cfg.ServerAddrs) > 0 {
		store := NewHTTPStore(cfg.ServerAddrs, cfg.HTTPTimeout, cfg.FailBackoff)
		return newClient(cfg, store, nil), nil
	}

	// MySQL 模式：直连数据库
	var db *sql.DB
	var closeDB func() error
	if cfg.DB != nil {
		db = cfg.DB
	} else {
		var err error
		db, err = sql.Open("mysql", cfg.DBConfig.DSN)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(cfg.DBConfig.MaxOpenConns)
		db.SetMaxIdleConns(cfg.DBConfig.MaxIdleConns)
		db.SetConnMaxLifetime(cfg.DBConfig.ConnMaxLifetime)
		if err := db.Ping(); err != nil {
			_ = db.Close()
			return nil, err
		}
		closeDB = db.Close
	}

	return newClient(cfg, NewMySQLStore(db), closeDB), nil
}

// NewClientWithServer 创建通过 HTTP 请求 Config Server 的客户端。
// serverAddrs 为 Server 地址列表，支持多节点轮询和故障转移。
func NewClientWithServer(serverAddrs []string) (*Client, error) {
	return NewClient(Config{ServerAddrs: serverAddrs})
}

// NewClientWithDSN 便捷入口，只传 DSN，SDK 内部使用默认连接池参数创建 db，Close 时自动关闭。
// 适合业务没有统一管理 *sql.DB 的简单场景。
func NewClientWithDSN(dsn string) (*Client, error) {
	return NewClient(Config{
		DBConfig: &DBConfig{DSN: dsn},
	})
}

func NewClientWithStore(store Store, refreshInterval, maxCacheTTL time.Duration) (*Client, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	cfg := Config{RefreshInterval: refreshInterval, MaxCacheTTL: maxCacheTTL}
	cfg.SetDefaults()
	return newClient(cfg, store, nil), nil
}

func newClient(cfg Config, store Store, closeDB func() error) *Client {
	return &Client{
		cfg:        cfg,
		store:      store,
		closeDB:    closeDB,
		cache:      make(map[ConfigIdentity]ConfigItem),
		typedRegs:  make(map[ConfigIdentity]unmarshalFunc),
		typedCache: make(map[ConfigIdentity]any),
	}
}

// Register 注册一个配置项的结构体/slice/map类型，SDK内部缓存反序列化后的对象。
// namespace + configKey 对应 GitHub 上的完整文件路径。
// 用法：
//
//	type RiskConfig struct { Enabled bool `json:"enabled"` }
//	riskCfg := client_sdk.Register[RiskConfig](client, "payment", "risk.json")
//	val, ok := riskCfg.Get()
func Register[T any](c *Client, namespace, configKey string) *TypedConfig[T] {
	identity := ConfigIdentity{Namespace: namespace, ConfigKey: configKey}

	unmarshal := func(value string) (any, error) {
		var obj T
		if err := json.Unmarshal([]byte(value), &obj); err != nil {
			var zero T
			return zero, err
		}
		return obj, nil
	}

	c.mu.Lock()
	c.typedRegs[identity] = unmarshal
	// 如果已经有缓存的字符串值，立即反序列化
	if item, ok := c.cache[identity]; ok && !item.Deleted && item.Value != "" {
		if obj, err := unmarshal(item.Value); err == nil {
			c.typedCache[identity] = obj
		}
	}
	c.mu.Unlock()

	return &TypedConfig[T]{
		client:   c,
		identity: identity,
	}
}

func (c *Client) Init(ctx context.Context) error {
	metaConfig, err := c.store.LoadMetaConfig(ctx, MetaConfig{
		RefreshInterval: c.cfg.RefreshInterval,
		MaxCacheTTL:     c.cfg.MaxCacheTTL,
	})
	if err == nil {
		c.applyMetaConfig(metaConfig)
	}

	snapshot, err := c.store.LoadSnapshot(ctx)
	if err != nil {
		return err
	}
	c.applySnapshot(snapshot, time.Now())
	c.mu.Lock()
	c.initialized = true
	c.mu.Unlock()
	return nil
}

func (c *Client) Start(ctx context.Context) error {
	if !c.isInitialized() {
		if err := c.Init(ctx); err != nil {
			return err
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.cancel = cancel
	c.mu.Unlock()

	go c.refreshLoop(runCtx)
	return nil
}

func (c *Client) Close() {
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	closeDB := c.closeDB
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if closeDB != nil {
		_ = closeDB()
	}
}

// GetConfig 返回配置字符串，配置不存在返回空字符串。
func (c *Client) GetConfig(namespace, configKey string) string {
	value, _ := c.GetConfigOK(namespace, configKey)
	return value
}

// GetConfigOK 返回配置字符串和是否存在。
func (c *Client) GetConfigOK(namespace, configKey string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	item, ok := c.cache[ConfigIdentity{Namespace: namespace, ConfigKey: configKey}]
	if !ok || item.Deleted {
		return "", false
	}
	return item.Value, true
}

func (c *Client) refreshLoop(ctx context.Context) {
	for {
		c.mu.RLock()
		interval := c.cfg.RefreshInterval
		c.mu.RUnlock()

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
			_ = c.refresh(ctx)
		}
	}
}

func (c *Client) applyMetaConfig(metaConfig MetaConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if metaConfig.RefreshInterval > 0 {
		c.cfg.RefreshInterval = metaConfig.RefreshInterval
	}
	if metaConfig.MaxCacheTTL > 0 {
		c.cfg.MaxCacheTTL = metaConfig.MaxCacheTTL
	}
}

func (c *Client) refresh(ctx context.Context) error {
	localVersion, lastCheck := c.versionState()
	if time.Since(lastCheck) < c.cfg.MaxCacheTTL {
		return nil
	}

	// 达到 TTL 后只查一行仓库整体版本，避免配置多时扫全表。
	remoteVersion, err := c.store.GetRepoVersion(ctx)
	if err != nil {
		c.setLastRefreshErr(err)
		return err
	}
	if remoteVersion == localVersion {
		c.updateLastVersionCheck(time.Now())
		c.setLastRefreshErr(nil)
		return nil
	}

	snapshot, err := c.store.LoadSnapshot(ctx)
	if err != nil {
		c.setLastRefreshErr(err)
		return err
	}
	c.applySnapshot(snapshot, time.Now())
	c.setLastRefreshErr(nil)
	return nil
}

// IsDegraded 返回当前是否处于降级状态（最近一次刷新失败且有旧缓存）。
func (c *Client) IsDegraded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRefreshErr != nil && c.initialized
}

// LastRefreshError 返回最近一次刷新的错误，无错误或从未刷新时返回 nil。
func (c *Client) LastRefreshError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRefreshErr
}

func (c *Client) setLastRefreshErr(err error) {
	c.mu.Lock()
	c.lastRefreshErr = err
	c.mu.Unlock()
}

func (c *Client) isInitialized() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.initialized
}

func (c *Client) versionState() (string, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.repoVersion, c.lastVersionCheck
}

func (c *Client) updateLastVersionCheck(t time.Time) {
	c.mu.Lock()
	c.lastVersionCheck = t
	c.mu.Unlock()
}

func (c *Client) applySnapshot(snapshot Snapshot, loadedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	next := make(map[ConfigIdentity]ConfigItem, len(snapshot.Items))
	nextTyped := make(map[ConfigIdentity]any, len(c.typedRegs))

	for identity, item := range snapshot.Items {
		item.LoadedAt = loadedAt
		next[identity] = item

		// 如果该配置项已注册类型，反序列化并缓存对象
		if unmarshal, registered := c.typedRegs[identity]; registered && !item.Deleted && item.Value != "" {
			if obj, err := unmarshal(item.Value); err == nil {
				nextTyped[identity] = obj
			}
		}
	}

	c.cache = next
	c.typedCache = nextTyped
	c.repoVersion = snapshot.RepoVersion
	c.lastVersionCheck = loadedAt
}

// getTypedObject 内部方法，供 TypedConfig 使用
func (c *Client) getTypedObject(identity ConfigIdentity) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	obj, ok := c.typedCache[identity]
	return obj, ok
}

// TypedConfig 是类型安全的配置句柄，由 Register 创建。
// Get 返回缓存的反序列化对象，配置版本更新时 SDK 自动重新解析并替换缓存。
// 返回的对象只读，业务侧请勿修改，否则会影响缓存。
type TypedConfig[T any] struct {
	client   *Client
	identity ConfigIdentity
}

// Get 返回缓存的反序列化对象和是否存在。
func (tc *TypedConfig[T]) Get() (T, bool) {
	var zero T
	obj, ok := tc.client.getTypedObject(tc.identity)
	if !ok {
		return zero, false
	}
	typed, ok := obj.(T)
	if !ok {
		return zero, false
	}
	return typed, true
}
