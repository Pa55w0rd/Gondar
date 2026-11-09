//go:build windows

package server

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func workspacePhysicalRoot(r *os.Root, _ string) (string, error) {
	f, err := r.Open(".")
	if err != nil {
		return "", err
	}
	defer f.Close()
	// EvalSymlinks does not expand junctions in all supported Go/Windows
	// configurations. Resolve the already-pinned handle instead of a new name.
	buf := make([]uint16, 1024)
	for len(buf) <= 32768 {
		n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if n >= uint32(len(buf)) {
			buf = make([]uint16, n+1)
			continue
		}
		path := windows.UTF16ToString(buf[:n])
		if strings.HasPrefix(path, `\\?\UNC\`) {
			path = `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
		} else {
			path = strings.TrimPrefix(path, `\\?\`)
		}
		return filepath.Clean(path), nil
	}
	return "", errWorkspaceBoundary
}
