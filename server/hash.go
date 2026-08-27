package server

import (
	"crypto/sha256"
	"fmt"
)

func ContentHash(content []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(content))
}

func RootPathHash(rootPath string) string {
	return ContentHash([]byte(normalizePath(rootPath)))
}
