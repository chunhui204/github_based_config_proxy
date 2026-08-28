package client_sdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	repoVersionFileName = ".repo_version"
	defaultGitHubBaseURL = "https://api.github.com"
)

// GitHubDumpConfig 从 GitHub 拉取配置所需的参数。
type GitHubDumpConfig struct {
	Token     string // GitHub Personal Access Token，需要私有仓库读取权限
	Owner     string // 仓库 owner，如 "chunhui204"
	Repo      string // 仓库名，如 "ads-dynamic-config"
	Branch    string // 分支名，默认 "main"
	RootPath  string // 配置文件所在的根目录，如 "configs"；为空则从仓库根目录开始
	OutputDir string // 本地输出目录
}

// DumpConfigFromGitHub 通过 GitHub API 直接拉取配置仓库中的全部文件到本地目录。
// 不需要 MySQL，适合本地开发时使用。配合 NewLocalFileStore 加载配置。
//
// 导出的目录结构：
//
//	OutputDir/
//	  .repo_version          # 记录当前 commit SHA
//	  <namespace>/
//	    <configKey>           # 文件内容为配置 JSON 字符串
func DumpConfigFromGitHub(ctx context.Context, cfg GitHubDumpConfig) error {
	if cfg.Token == "" {
		return fmt.Errorf("github token is required")
	}
	if cfg.Owner == "" || cfg.Repo == "" {
		return fmt.Errorf("github owner and repo are required")
	}
	if cfg.OutputDir == "" {
		return fmt.Errorf("output dir is required")
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	cfg.RootPath = normalizeDumpPath(cfg.RootPath)

	client := &gitHubDumper{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		baseURL:    defaultGitHubBaseURL,
		token:      cfg.Token,
		owner:      cfg.Owner,
		repo:       cfg.Repo,
	}

	// 1. 获取 head commit SHA
	commitSHA, err := client.getHeadCommit(ctx, cfg.Branch)
	if err != nil {
		return fmt.Errorf("get head commit: %w", err)
	}

	// 2. 递归获取文件树，筛选 rootPath 下的文件
	files, err := client.listFiles(ctx, commitSHA, cfg.RootPath)
	if err != nil {
		return fmt.Errorf("list files: %w", err)
	}

	// 3. 清理并重建输出目录
	if err := os.RemoveAll(cfg.OutputDir); err != nil {
		return fmt.Errorf("clean output dir: %w", err)
	}
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	// 4. 写入版本号
	if err := os.WriteFile(
		filepath.Join(cfg.OutputDir, repoVersionFileName),
		[]byte(commitSHA), 0o644,
	); err != nil {
		return fmt.Errorf("write version file: %w", err)
	}

	// 5. 逐一下载配置文件并写入本地
	for _, file := range files {
		content, err := client.getFileContent(ctx, file.Path, commitSHA)
		if err != nil {
			return fmt.Errorf("download %s: %w", file.Path, err)
		}

		// 将 GitHub 路径（configs/payment/risk.json）映射为本地路径（payment/risk.json）
		relPath := file.Path
		if cfg.RootPath != "" {
			relPath = strings.TrimPrefix(relPath, cfg.RootPath+"/")
		}
		if relPath == "" || strings.Contains(relPath, "..") {
			continue
		}

		localPath := filepath.Join(cfg.OutputDir, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", relPath, err)
		}
		if err := os.WriteFile(localPath, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", relPath, err)
		}
	}

	return nil
}

// gitHubDumper 封装 GitHub REST API 调用，仅用于本地 dump。
type gitHubDumper struct {
	httpClient *http.Client
	baseURL    string
	token      string
	owner      string
	repo       string
}

type gitHubFile struct {
	Path string
}

func (d *gitHubDumper) getHeadCommit(ctx context.Context, branch string) (string, error) {
	var resp struct {
		SHA string `json:"sha"`
	}
	endpoint := d.repoURL("commits/" + url.PathEscape(branch))
	if err := d.getJSON(ctx, endpoint, &resp); err != nil {
		return "", err
	}
	if resp.SHA == "" {
		return "", fmt.Errorf("head commit sha is empty")
	}
	return resp.SHA, nil
}

func (d *gitHubDumper) listFiles(ctx context.Context, commitSHA, rootPath string) ([]gitHubFile, error) {
	var resp struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	endpoint := d.repoURL("git/trees/" + url.PathEscape(commitSHA) + "?recursive=1")
	if err := d.getJSON(ctx, endpoint, &resp); err != nil {
		return nil, err
	}
	if resp.Truncated {
		return nil, fmt.Errorf("github tree is truncated, too many files")
	}

	prefix := rootPath + "/"
	files := make([]gitHubFile, 0, len(resp.Tree))
	for _, item := range resp.Tree {
		if item.Type != "blob" {
			continue
		}
		if rootPath != "" && !strings.HasPrefix(item.Path, prefix) {
			continue
		}
		files = append(files, gitHubFile{Path: item.Path})
	}
	return files, nil
}

func (d *gitHubDumper) getFileContent(ctx context.Context, filePath, commitSHA string) ([]byte, error) {
	var resp struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	endpoint := d.repoURL("contents/" + escapePath(filePath) + "?ref=" + url.QueryEscape(commitSHA))
	if err := d.getJSON(ctx, endpoint, &resp); err != nil {
		return nil, err
	}
	if resp.Encoding != "base64" {
		return nil, fmt.Errorf("unsupported encoding %q", resp.Encoding)
	}
	raw := strings.NewReplacer("\n", "", "\r", "").Replace(resp.Content)
	return base64.StdEncoding.DecodeString(raw)
}

func (d *gitHubDumper) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	return json.Unmarshal(body, out)
}

func (d *gitHubDumper) repoURL(apiPath string) string {
	return fmt.Sprintf("%s/repos/%s/%s/%s",
		d.baseURL,
		url.PathEscape(d.owner),
		url.PathEscape(d.repo),
		apiPath,
	)
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func normalizeDumpPath(p string) string {
	return strings.Trim(strings.TrimSpace(p), "/")
}

// LocalFileStore 从本地目录加载配置，实现 Store 接口，用于本地开发测试。
// 目录结构由 DumpConfigFromGitHub 生成，也可以直接 clone 配置仓库后指定子目录。
type LocalFileStore struct {
	dir string
}

// NewLocalFileStore 创建一个从本地目录读取配置的 Store。
// dir 应包含 <namespace>/<configKey> 的子目录结构和 .repo_version 文件。
func NewLocalFileStore(dir string) *LocalFileStore {
	return &LocalFileStore{dir: dir}
}

func (s *LocalFileStore) GetRepoVersion(_ context.Context) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, repoVersionFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return "local", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (s *LocalFileStore) LoadMetaConfig(_ context.Context, cfg MetaConfig) (MetaConfig, error) {
	return cfg, nil
}

func (s *LocalFileStore) LoadSnapshot(ctx context.Context) (Snapshot, error) {
	repoVersion, err := s.GetRepoVersion(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	items := make(map[ConfigIdentity]ConfigItem)
	now := time.Now()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read dir %s: %w", s.dir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		namespace := entry.Name()
		configDir := filepath.Join(s.dir, namespace)

		configFiles, err := os.ReadDir(configDir)
		if err != nil {
			return Snapshot{}, fmt.Errorf("read namespace dir %s: %w", configDir, err)
		}

		for _, cf := range configFiles {
			if cf.IsDir() {
				continue
			}
			configKey := cf.Name()
			content, err := os.ReadFile(filepath.Join(configDir, configKey))
			if err != nil {
				return Snapshot{}, fmt.Errorf("read config %s/%s: %w", namespace, configKey, err)
			}
			items[ConfigIdentity{Namespace: namespace, ConfigKey: configKey}] = ConfigItem{
				Value:    string(content),
				LoadedAt: now,
			}
		}
	}

	return Snapshot{
		RepoVersion: repoVersion,
		Items:       items,
	}, nil
}
