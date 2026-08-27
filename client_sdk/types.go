package client_sdk

import "time"

type ConfigIdentity struct {
	Namespace string
	ConfigKey string
}

type ConfigItem struct {
	Value    string
	Deleted  bool
	LoadedAt time.Time
}

type Snapshot struct {
	RepoVersion string
	Items       map[ConfigIdentity]ConfigItem
}
