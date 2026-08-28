package server

import (
	"context"
	"fmt"
	"sync"
)

// FetchedConfig 表示从 GitHub 拉取到的一个配置文件。
type FetchedConfig struct {
	Identity ConfigIdentity
	Path     string
	Content  []byte
}

// ListGitHubConfigs 拉取指定 commit 下 rootPath 内的所有配置文件。
// 并发下载文件内容（并发度上限 8），返回以 ConfigIdentity 为 key 的 map。
// Syncer 和 GitHubSnapshotReader 共用此函数，避免重复 GitHub API 调用和路径解析逻辑。
func ListGitHubConfigs(ctx context.Context, client GitHubClient, rootPath, commitSHA string) (map[ConfigIdentity]FetchedConfig, error) {
	files, err := client.ListFiles(ctx, commitSHA)
	if err != nil {
		return nil, err
	}

	type result struct {
		identity ConfigIdentity
		config   FetchedConfig
		err      error
	}

	// 带并发上限的 goroutine 池，避免一次性发出过多 HTTP 请求
	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	resultCh := make(chan result, len(files))

	for _, file := range files {
		identity, ok := IdentityFromGitHubPath(rootPath, file.Path)
		if !ok {
			continue
		}

		wg.Add(1)
		go func(filePath string, identity ConfigIdentity) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			content, err := client.GetFileContent(ctx, filePath, commitSHA)
			resultCh <- result{
				identity: identity,
				config: FetchedConfig{
					Identity: identity,
					Path:     filePath,
					Content:  content,
				},
				err: err,
			}
		}(file.Path, identity)
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	configs := make(map[ConfigIdentity]FetchedConfig)
	for res := range resultCh {
		if res.err != nil {
			return nil, fmt.Errorf("fetch %s: %w", res.config.Path, res.err)
		}
		configs[res.identity] = res.config
	}
	return configs, nil
}

// GitHubSnapshotReader 直接从 GitHub 拉取全量配置，实现 SnapshotReader 接口。
// 用于 Server 本地模式，不依赖 MySQL。
type GitHubSnapshotReader struct {
	client   GitHubClient
	rootPath string
}

// NewGitHubSnapshotReader 创建一个从 GitHub 读取配置快照的 Reader。
func NewGitHubSnapshotReader(client GitHubClient, rootPath string) *GitHubSnapshotReader {
	return &GitHubSnapshotReader{
		client:   client,
		rootPath: normalizePath(rootPath),
	}
}

// GetRepoVersion 返回 GitHub HEAD commit SHA 作为版本号。
func (r *GitHubSnapshotReader) GetRepoVersion(ctx context.Context) (string, error) {
	return r.client.GetHeadCommit(ctx)
}

// LoadSnapshot 拉取 GitHub 上当前所有配置文件，组装为 CacheSnapshot。
func (r *GitHubSnapshotReader) LoadSnapshot(ctx context.Context) (CacheSnapshot, error) {
	commitSHA, err := r.client.GetHeadCommit(ctx)
	if err != nil {
		return CacheSnapshot{}, err
	}

	configs, err := ListGitHubConfigs(ctx, r.client, r.rootPath, commitSHA)
	if err != nil {
		return CacheSnapshot{}, err
	}

	items := make(map[ConfigIdentity]CachedItem, len(configs))
	for identity, cfg := range configs {
		items[identity] = CachedItem{Value: string(cfg.Content)}
	}
	return CacheSnapshot{RepoVersion: commitSHA, Items: items}, nil
}
