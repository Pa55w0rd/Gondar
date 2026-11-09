//go:build !windows

package hostd

import (
	"os"
	"syscall"
)

func installerOwnsFile(st os.FileInfo) bool {
	stat, ok := st.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
