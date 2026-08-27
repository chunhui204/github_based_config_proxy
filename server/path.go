package server

import (
	"path"
	"strings"
)

func IdentityFromGitHubPath(rootPath, filePath string) (ConfigIdentity, bool) {
	root := normalizePath(rootPath)
	file := normalizePath(filePath)
	if root != "" {
		prefix := root + "/"
		if !strings.HasPrefix(file, prefix) {
			return ConfigIdentity{}, false
		}
		file = strings.TrimPrefix(file, prefix)
	}

	namespace, configKey := path.Split(file)
	namespace = strings.TrimSuffix(namespace, "/")
	if namespace == "" || configKey == "" {
		return ConfigIdentity{}, false
	}
	return ConfigIdentity{Namespace: namespace, ConfigKey: configKey}, true
}

func normalizePath(value string) string {
	return strings.Trim(strings.TrimSpace(value), "/")
}
