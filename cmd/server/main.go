package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github_based_config_proxy/server"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	mode := flag.String("mode", server.ModeMySQL, "运行模式: mysql（默认，连MySQL+选主同步）或 local（直连GitHub，无MySQL）")
	configPath := flag.String("config", "config/server.json", "config file path")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var (
		cfg         server.Config
		snapshotRdr server.SnapshotReader
	)

	switch *mode {
	case server.ModeLocal:
		var err error
		cfg, err = server.NewLocalConfigFromFile(*configPath)
		if err != nil {
			log.Fatalf("load local config failed: %v", err)
		}
		cfg.SetDefaults()
		if err := cfg.ValidateLocal(); err != nil {
			log.Fatalf("local config validate failed: %v", err)
		}
		// 本地模式：直接从 GitHub 拉取配置，不连 MySQL、不选主
		githubClient := server.NewGitHubAPIClient(cfg, nil)
		snapshotRdr = server.NewGitHubSnapshotReader(githubClient, cfg.GitHubRootPath)
		log.Printf("running in LOCAL mode: github=%s/%s branch=%s root=%s",
			cfg.GitHubOwner, cfg.GitHubRepo, cfg.GitHubBranch, cfg.GitHubRootPath)

	default:
		var err error
		cfg, err = server.NewConfigFromFile(*configPath)
		if err != nil {
			log.Fatalf("load database config failed: %v", err)
		}

		db, err := sql.Open("mysql", cfg.MySQLDSN())
		if err != nil {
			log.Fatalf("open mysql failed: %v", err)
		}
		defer db.Close()

		db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
		db.SetMaxIdleConns(cfg.Database.MaxIdleConns)
		db.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)

		if err := db.PingContext(ctx); err != nil {
			log.Fatalf("ping mysql failed: %v", err)
		}
		log.Printf("mysql connected: %s:%d/%s", cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName)

		store := server.NewMySQLStore(db)

		// 从 META 表加载所有 github/server/client 配置
		cfg, err = store.LoadConfigFromMeta(ctx, cfg)
		if err != nil {
			log.Fatalf("load config from meta failed: %v (please insert config rows into ADS_SERVICE_DYNAMIC_CONFIG_META first)", err)
		}
		cfg.SetDefaults()
		if err := cfg.Validate(); err != nil {
			log.Fatalf("config validate failed: %v", err)
		}

		syncer, err := server.NewSyncer(cfg, store, nil)
		if err != nil {
			log.Fatalf("create syncer failed: %v", err)
		}
		if err := syncer.Start(ctx); err != nil {
			log.Fatalf("start syncer failed: %v", err)
		}

		// MySQL 模式：缓存从 MySQL 读取快照
		snapshotRdr = store
		log.Printf("running in MYSQL mode: instance_name=%s, sync_interval=%s, lock_ttl=%s",
			cfg.InstanceID(), cfg.SyncInterval, cfg.LockLeaseTTL)
	}

	// 创建内存缓存并启动定期刷新（两种模式共用）
	cache := server.NewConfigCache()
	refresher := server.NewCacheRefresher(snapshotRdr, cache, cfg.CacheRefreshInterval)
	if err := refresher.Start(ctx); err != nil {
		log.Fatalf("start cache refresher failed: %v", err)
	}

	// 启动 HTTP 服务，所有接口从内存缓存读取
	httpServer := &http.Server{
		Addr:    cfg.ListenAddr(),
		Handler: server.NewConfigHandler(cache, cfg.Client),
	}
	go func() {
		log.Printf("http server listening on %s (cache_refresh_interval=%s)",
			cfg.ListenAddr(), cfg.CacheRefreshInterval)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("server shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	time.Sleep(200 * time.Millisecond)
}
