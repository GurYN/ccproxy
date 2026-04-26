package bridgeclient

import (
	"io/fs"
	"os"
	"path/filepath"
)

// pathInfo and pathStat are tiny adapters so watcher.go can stay
// platform-agnostic without importing os twice.

type pathInfo = fs.FileInfo

func pathStat(p string) (pathInfo, error) {
	return os.Stat(p)
}

// Compile-time check that filepath.Walk's WalkFunc arg matches our alias.
var _ filepath.WalkFunc = func(string, pathInfo, error) error { return nil }
