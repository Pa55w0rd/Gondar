package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// saveChatUpload reserves the final name with O_EXCL. Both reservation and I/O
// use the pinned workspace directory, never an absolute path checked earlier.
func saveChatUpload(ws *workspaceRoot, dir, name string, src io.Reader) (string, int64, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", 0, errWorkspaceBoundary
	}
	target, err := ws.resolve(filepath.Join(dir, name), true, false, true)
	if err != nil {
		return "", 0, err
	}
	defer target.close()
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		out, err := target.parent.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", 0, err
		}
		created, statErr := out.Stat()
		var size int64
		if statErr == nil {
			size, err = io.Copy(out, src)
		}
		err = errors.Join(statErr, err, out.Close())
		if err != nil {
			// A failed copy removes only the newly reserved attachment. Do not
			// remove a different file if a local process replaced that name.
			if current, e := target.parent.Lstat(candidate); e == nil && created != nil && os.SameFile(created, current) {
				err = errors.Join(err, target.parent.Remove(candidate))
			}
			return "", size, err
		}
		return candidate, size, nil
	}
}
