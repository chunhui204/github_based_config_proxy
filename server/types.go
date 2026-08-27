package server

type RepoIdentity struct {
	Owner    string
	Repo     string
	Branch   string
	RootPath string
}

type ConfigIdentity struct {
	Namespace string
	ConfigKey string
}

type CurrentRecord struct {
	Identity        ConfigIdentity
	Path            string
	ContentHash     string
	Deleted         bool
	GitHubCommitSHA string
}

type SyncConfigItem struct {
	Identity        ConfigIdentity
	Path            string
	Content         string
	ContentHash     string
	GitHubCommitSHA string
}

type DeletedConfigItem struct {
	Identity        ConfigIdentity
	Path            string
	GitHubCommitSHA string
}

type GitHubFile struct {
	Path string
}
