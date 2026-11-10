package hostd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gondar/hosttools"
)

func resolveGem(ctx context.Context, client *http.Client, tool hosttools.Tool, info *ToolVersion) (*hosttools.InstallPin, error) {
	info.Source = "https://rubygems.org/gems/" + tool.Gem
	raw, err := metadata(ctx, client, "https://rubygems.org/api/v1/gems/"+tool.Gem+".json", 1<<20)
	if err != nil {
		return nil, err
	}
	var gem struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		SHA     string `json:"sha"`
	}
	if json.Unmarshal(raw, &gem) != nil || gem.Name != tool.Gem || !exactVersion.MatchString(gem.Version) || strings.Contains(gem.Version, "..") || !digestPattern.MatchString(gem.SHA) {
		return nil, fmt.Errorf("官方 RubyGems 没有返回有效版本与 SHA256")
	}
	info.LatestVersion, info.SHA256 = gem.Version, strings.ToLower(gem.SHA)
	return &hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Package: tool.Gem, Version: gem.Version, SHA256: info.SHA256}, nil
}

func (s *Server) runGem(ctx context.Context, j *Job, tool hosttools.Tool, proxy string) (int, error) {
	if j.installPin == nil || j.installPin.Kind != hosttools.KindGem || j.installPin.Package != tool.Gem || tool.ID != "evil-winrm" {
		return 1, fmt.Errorf("RubyGem 版本计划无效，请重新检测")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	client := s.archiveClient(proxy)
	defer client.CloseIdleConnections()
	address := "https://rubygems.org/downloads/" + tool.Gem + "-" + url.PathEscape(j.installPin.Version) + ".gem"
	blob, err := fetchToolArchive(ctx, client, address)
	if err != nil {
		return 1, err
	}
	if err = verifyToolArchive(blob, *j.installPin); err != nil {
		return 1, err
	}
	if _, err := exec.LookPath("ruby"); err != nil {
		if code, err := s.bootstrap(ctx, j, proxy, "ruby ruby-dev build-essential libreadline-dev"); err != nil {
			return code, err
		}
	}
	// Native gem dependencies need Ruby/readline headers, which runtime Ruby packages omit.
	probe := exec.CommandContext(ctx, "ruby", "-rrbconfig", "-e", "print RbConfig::CONFIG['rubyhdrdir']")
	header, _ := probe.Output()
	if _, err := os.Stat(filepath.Join(strings.TrimSpace(string(header)), "ruby.h")); err != nil {
		if code, err := s.bootstrap(ctx, j, proxy, "ruby-dev build-essential libreadline-dev"); err != nil {
			return code, err
		}
	}
	root := filepath.Join(filepath.Dir(j.installBinDir()), "share", tool.ID, j.installPin.Version)
	if err = os.MkdirAll(root, 0o755); err != nil {
		return 1, err
	}
	archive := filepath.Join(root, tool.Gem+".gem")
	if err = os.WriteFile(archive, blob, 0o600); err != nil {
		return 1, err
	}
	home := filepath.Join(root, "gems")
	args := []string{"install", archive, "--config-file", "/dev/null", "--no-document", "--clear-sources", "--source", "https://rubygems.org", "--install-dir", home, "--bindir", filepath.Join(home, "bin")}
	env := []string{"GEM_HOME=" + home, "GEM_PATH=" + home, "HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy}
	if code, err := s.execLogged(ctx, j, "gem", args, env); err != nil {
		return code, err
	}
	entry := filepath.Join(home, "bin", tool.Bin)
	if st, err := os.Stat(entry); err != nil || st.IsDir() {
		return 1, fmt.Errorf("RubyGem 安装后没有生成命令入口")
	}
	wrapper := "#!/bin/sh\nexport GEM_HOME=" + shellArgument(home) + "\nexport GEM_PATH=\"$GEM_HOME\"\nexec " + shellArgument(entry) + " \"$@\"\n"
	return installBinary(j, tool, []byte(wrapper))
}
