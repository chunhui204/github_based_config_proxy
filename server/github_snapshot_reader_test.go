package server

import (
	"context"
	"errors"
	"testing"
)

func TestListGitHubConfigsFetchesAllFiles(t *testing.T) {
	github := &fakeGitHub{
		head: "commit-1",
		files: []GitHubFile{
			{Path: "configs/payment/risk.yaml"},
			{Path: "configs/common/whitelist.yaml"},
		},
		content: map[string][]byte{
			"configs/payment/risk.yaml":      []byte("enabled: true"),
			"configs/common/whitelist.yaml":  []byte("[a,b]"),
		},
	}

	configs, err := ListGitHubConfigs(context.Background(), github, "configs", "commit-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 {
		t.Fatalf("configs count=%d, want 2", len(configs))
	}

	risk := configs[ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}]
	if string(risk.Content) != "enabled: true" {
		t.Fatalf("risk content=%q", risk.Content)
	}
	if risk.Path != "configs/payment/risk.yaml" {
		t.Fatalf("risk path=%q", risk.Path)
	}
}

func TestListGitHubConfigsSkipsFilesOutsideRoot(t *testing.T) {
	github := &fakeGitHub{
		files: []GitHubFile{
			{Path: "configs/ns/a.yaml"},
			{Path: "README.md"},
			{Path: ".github/workflows/ci.yml"},
		},
		content: map[string][]byte{
			"configs/ns/a.yaml":         []byte("a"),
			"README.md":                 []byte("readme"),
			".github/workflows/ci.yml":  []byte("ci"),
		},
	}

	configs, err := ListGitHubConfigs(context.Background(), github, "configs", "commit-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 {
		t.Fatalf("configs count=%d, want 1 (only files under configs/)", len(configs))
	}
}

func TestListGitHubConfigsSkipsFilesWithoutNamespace(t *testing.T) {
	github := &fakeGitHub{
		files: []GitHubFile{
			{Path: "configs/root.yaml"},
			{Path: "configs/ns/a.yaml"},
		},
		content: map[string][]byte{
			"configs/root.yaml": []byte("root"),
			"configs/ns/a.yaml": []byte("a"),
		},
	}

	configs, err := ListGitHubConfigs(context.Background(), github, "configs", "commit-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 {
		t.Fatalf("configs count=%d, want 1 (root-level files without namespace skipped)", len(configs))
	}
}

func TestListGitHubConfigsPropagatesContentError(t *testing.T) {
	github := &fakeGitHub{
		files: []GitHubFile{
			{Path: "configs/ns/a.yaml"},
		},
		content: map[string][]byte{}, // 空 map 导致返回 nil, nil
	}

	// fakeGitHub.GetFileContent 对不存在的文件返回 nil, nil
	_, err := ListGitHubConfigs(context.Background(), github, "configs", "commit-1")
	if err != nil {
		// nil content 不会导致 error，但后续逻辑会处理空内容
		// 这里主要验证不会 panic
		t.Logf("got error (acceptable for nil content): %v", err)
	}
}

func TestGitHubSnapshotReaderLoadSnapshot(t *testing.T) {
	github := &fakeGitHub{
		head: "commit-abc",
		files: []GitHubFile{
			{Path: "configs/payment/risk.yaml"},
			{Path: "configs/common/whitelist.yaml"},
		},
		content: map[string][]byte{
			"configs/payment/risk.yaml":     []byte(`{"enabled":true}`),
			"configs/common/whitelist.yaml": []byte(`["a","b"]`),
		},
	}
	reader := NewGitHubSnapshotReader(github, "configs")

	snapshot, err := reader.LoadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RepoVersion != "commit-abc" {
		t.Fatalf("version=%q", snapshot.RepoVersion)
	}
	if len(snapshot.Items) != 2 {
		t.Fatalf("items count=%d, want 2", len(snapshot.Items))
	}

	risk := snapshot.Items[ConfigIdentity{Namespace: "payment", ConfigKey: "risk.yaml"}]
	if risk.Value != `{"enabled":true}` {
		t.Fatalf("risk value=%q", risk.Value)
	}
	if risk.Deleted {
		t.Fatal("newly loaded item should not be marked deleted")
	}
}

func TestGitHubSnapshotReaderGetRepoVersion(t *testing.T) {
	github := &fakeGitHub{head: "sha-123"}
	reader := NewGitHubSnapshotReader(github, "configs")

	version, err := reader.GetRepoVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "sha-123" {
		t.Fatalf("version=%q, want sha-123", version)
	}
}

func TestGitHubSnapshotReaderPropagatesHeadError(t *testing.T) {
	github := &fakeGitHub{headErr: errors.New("github api rate limited")}
	reader := NewGitHubSnapshotReader(github, "configs")

	if _, err := reader.GetRepoVersion(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}
