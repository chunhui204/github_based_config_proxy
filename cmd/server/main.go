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
	configPath := flag.String("config", "config/server.json", "config file path")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 第一步：从配置文件读取数据库连接配置（连接MySQL必需）
	cfg, err := server.NewConfigFromFile(*configPath)
	if err != nil {
		log.Fatalf("load database config failed: %v", err)
	}

	// 第二步：连接MySQL
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

	// 第三步：从META表加载所有github/server/client配置
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

	// 启动 HTTP 健康检查服务（容器内固定监听8080）
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	httpServer := &http.Server{
		Addr:    cfg.ListenAddr(),
		Handler: mux,
	}
	go func() {
		log.Printf("config sync server started, instance_name=%s, sync_interval=%s, lock_ttl=%s",
			cfg.InstanceID(), cfg.SyncInterval, cfg.LockLeaseTTL)
		log.Printf("http server listening on %s", cfg.ListenAddr())
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
