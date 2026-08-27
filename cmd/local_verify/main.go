package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	client_sdk "github_based_config_proxy/client_sdk"
	"github_based_config_proxy/server"

	_ "github.com/go-sql-driver/mysql"
)

const (
	testNamespace = "payment"
	testConfigKey = "risk.yaml"
	testValue     = "enabled: true"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18080", "local http service address")
	requests := flag.Int("requests", 1000, "total stress requests")
	concurrency := flag.Int("concurrency", 50, "stress concurrency")
	serveOnly := flag.Bool("serve-only", false, "keep local http service running")
	useMySQL := flag.Bool("mysql", false, "read config from mysql")
	configPath := flag.String("config", "config/server.json", "server config file path")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client, cleanup, err := newClient(ctx, *useMySQL, *configPath)
	if err != nil {
		panic(err)
	}
	defer cleanup()
	if initErr := client.Init(ctx); initErr != nil {
		panic(initErr)
	}

	server := newHTTPServer(*addr, client)
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		panic(err)
	}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			panic(err)
		}
	}()
	defer server.Shutdown(context.Background())

	if *serveOnly {
		fmt.Printf("listening on http://%s\n", listener.Addr().String())
		<-ctx.Done()
		return
	}

	if err := stress(ctx, "http://"+listener.Addr().String()+"/config?namespace=payment&key=risk.yaml", testValue, *requests, *concurrency); err != nil {
		panic(err)
	}
	fmt.Printf("ok requests=%d concurrency=%d value=%q\n", *requests, *concurrency, testValue)
}

func newClient(ctx context.Context, useMySQL bool, configPath string) (*client_sdk.Client, func(), error) {
	if !useMySQL {
		store := newMemoryStore(testNamespace, testConfigKey, testValue)
		client, err := client_sdk.NewClientWithStore(store, time.Second, time.Second)
		return client, func() {}, err
	}

	cfg, err := server.NewConfigFromFile(configPath)
	if err != nil {
		return nil, nil, err
	}
	db, err := sql.Open("mysql", cfg.MySQLDSN())
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, nil, err
	}

	client, err := client_sdk.NewClient(client_sdk.Config{
		DB:              db,
		RefreshInterval: time.Second,
		MaxCacheTTL:     time.Second,
	})
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return client, func() { db.Close() }, nil
}

func newHTTPServer(addr string, client *client_sdk.Client) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		namespace := r.URL.Query().Get("namespace")
		configKey := r.URL.Query().Get("key")
		value, ok := client.GetConfigOK(namespace, configKey)
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, value)
	})
	return &http.Server{Addr: addr, Handler: mux}
}

func stress(ctx context.Context, endpoint, expected string, requests, concurrency int) error {
	if requests <= 0 || concurrency <= 0 {
		return fmt.Errorf("requests and concurrency must be positive")
	}

	jobs := make(chan int)
	errs := make(chan error, concurrency)
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				if err := requestOnce(ctx, endpoint, expected); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	for i := 0; i < requests; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func requestOnce(ctx context.Context, endpoint, expected string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	if string(body) != expected {
		return fmt.Errorf("value=%q want %q", string(body), expected)
	}
	return nil
}

type memoryStore struct {
	snapshot client_sdk.Snapshot
}

func newMemoryStore(namespace, configKey, value string) *memoryStore {
	return &memoryStore{
		snapshot: client_sdk.Snapshot{
			RepoVersion: "local-commit-1",
			Items: map[client_sdk.ConfigIdentity]client_sdk.ConfigItem{
				{Namespace: namespace, ConfigKey: configKey}: {Value: value},
			},
		},
	}
}

func (s *memoryStore) LoadSnapshot(context.Context) (client_sdk.Snapshot, error) {
	items := make(map[client_sdk.ConfigIdentity]client_sdk.ConfigItem, len(s.snapshot.Items))
	for identity, item := range s.snapshot.Items {
		items[identity] = item
	}
	return client_sdk.Snapshot{RepoVersion: s.snapshot.RepoVersion, Items: items}, nil
}

func (s *memoryStore) GetRepoVersion(context.Context) (string, error) {
	return s.snapshot.RepoVersion, nil
}

func (s *memoryStore) LoadMetaConfig(_ context.Context, cfg client_sdk.MetaConfig) (client_sdk.MetaConfig, error) {
	return cfg, nil
}
