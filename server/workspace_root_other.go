//go:build !windows

package server

import (
	"os"
	"path/filepath"
)

func workspacePhysicalRoot(_ *os.Root, configured string) (string, error) {
	return filepath.EvalSymlinks(configured)
}
