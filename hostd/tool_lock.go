package hostd

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"gondar/hosttools"
)

const DefaultToolLockPath = "/etc/gondar/tool-install-lock.json"

func (s *Server) pinnedTool(tool hosttools.Tool) (hosttools.InstallPin, error) {
	filename := s.ToolLockPath
	if filename == "" {
		filename = DefaultToolLockPath
	}
	f, err := os.Open(filename)
	if err != nil {
		return hosttools.InstallPin{}, fmt.Errorf("尚未配置受信工具安装清单；请由管理员审核确切版本及归档摘要后配置 %s", DefaultToolLockPath)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || (runtime.GOOS != "windows" && st.Mode().Perm()&0o022 != 0) || !installerOwnsFile(st) {
		return hosttools.InstallPin{}, fmt.Errorf("工具安装清单必须为安装助手用户拥有、且组与其他用户不可写的普通文件")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return hosttools.InstallPin{}, fmt.Errorf("工具安装清单读取失败")
	}
	lock, err := hosttools.ParseInstallLock(raw)
	if err != nil {
		return hosttools.InstallPin{}, err
	}
	return lock.Pin(tool, runtime.GOOS, runtime.GOARCH)
}

func verifyToolArchive(blob []byte, pin hosttools.InstallPin) error {
	sum := sha256.Sum256(blob)
	if !strings.EqualFold(fmt.Sprintf("%x", sum), pin.SHA256) {
		return fmt.Errorf("下载内容的 SHA256 与检测到的确切版本不一致，拒绝安装")
	}
	return nil
}
