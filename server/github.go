package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type GitHubClient interface {
	GetHeadCommit(ctx context.Context) (string, error)
	ListFiles(ctx context.Context, commitSHA string) ([]GitHubFile, error)
	GetFileContent(ctx context.Context, filePath string, commitSHA string) ([]byte, error)
}

type GitHubAPIClient struct {
	httpClient *http.Client
	baseURL    string
	token      string
	owner      string
	repo       string
	branch     string
	rootPath   string
}

func NewGitHubAPIClient(cfg Config, httpClient *http.Client) *GitHubAPIClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if cfg.GitHubBaseURL == "" {
		cfg.GitHubBaseURL = DefaultGitHubBaseURL
	}
	if cfg.GitHubBranch == "" {
		cfg.GitHubBranch = "main"
	}
	return &GitHubAPIClient{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(cfg.GitHubBaseURL, "/"),
		token:      cfg.GitHubToken,
		owner:      cfg.GitHubOwner,
		repo:       cfg.GitHubRepo,
		branch:     cfg.GitHubBranch,
		rootPath:   normalizePath(cfg.GitHubRootPath),
	}
}

func (c *GitHubAPIClient) GetHeadCommit(ctx context.Context) (string, error) {
	var response struct {
		SHA string `json:"sha"`
	}
	endpoint := c.repoURL("commits/" + url.PathEscape(c.branch))
	if err := c.getJSON(ctx, endpoint, &response); err != nil {
		return "", err
	}
	if response.SHA == "" {
		return "", fmt.Errorf("github head commit is empty")
	}
	return response.SHA, nil
}

func (c *GitHubAPIClient) ListFiles(ctx context.Context, commitSHA string) ([]GitHubFile, error) {
	treeSHA, err := c.getCommitTreeSHA(ctx, commitSHA)
	if err != nil {
		return nil, err
	}

	var response struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	endpoint := c.repoURL("git/trees/" + url.PathEscape(treeSHA) + "?recursive=1")
	if err := c.getJSON(ctx, endpoint, &response); err != nil {
		return nil, err
	}
	if response.Truncated {
		return nil, fmt.Errorf("github tree is truncated")
	}

	rootPrefix := c.rootPath + "/"
	files := make([]GitHubFile, 0, len(response.Tree))
	for _, item := range response.Tree {
		if item.Type != "blob" || !strings.HasPrefix(item.Path, rootPrefix) {
			continue
		}
		files = append(files, GitHubFile{Path: item.Path})
	}
	return files, nil
}

func (c *GitHubAPIClient) GetFileContent(ctx context.Context, filePath string, commitSHA string) ([]byte, error) {
	var response struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	endpoint := c.repoURL("contents/" + escapeGitHubPath(filePath) + "?ref=" + url.QueryEscape(commitSHA))
	if err := c.getJSON(ctx, endpoint, &response); err != nil {
		return nil, err
	}
	if response.Encoding != "base64" {
		return nil, fmt.Errorf("unsupported github content encoding %q", response.Encoding)
	}
	raw := strings.NewReplacer("\n", "", "\r", "").Replace(response.Content)
	return base64.StdEncoding.DecodeString(raw)
}

func (c *GitHubAPIClient) getCommitTreeSHA(ctx context.Context, commitSHA string) (string, error) {
	var response struct {
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	endpoint := c.repoURL("commits/" + url.PathEscape(commitSHA))
	if err := c.getJSON(ctx, endpoint, &response); err != nil {
		return "", err
	}
	if response.Commit.Tree.SHA == "" {
		return "", fmt.Errorf("github commit tree sha is empty")
	}
	return response.Commit.Tree.SHA, nil
}

func (c *GitHubAPIClient) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("github api %s failed: status=%d body=%s", endpoint, resp.StatusCode, string(body))
	}
	return json.Unmarshal(body, out)
}

func (c *GitHubAPIClient) repoURL(apiPath string) string {
	return fmt.Sprintf("%s/repos/%s/%s/%s", c.baseURL, url.PathEscape(c.owner), url.PathEscape(c.repo), apiPath)
}

func escapeGitHubPath(filePath string) string {
	parts := strings.Split(filePath, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
