package hostd

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gondar/hosttools"
)

var gitCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

func parseRemoteHead(raw string) (string, error) {
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == "HEAD" && gitCommit.MatchString(parts[0]) {
			return parts[0], nil
		}
	}
	return "", fmt.Errorf("官方字典仓库没有返回有效提交")
}

func gitEnvironment(proxy string) []string {
	return []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never", "GIT_LFS_SKIP_SMUDGE=1", "HTTPS_PROXY=" + proxy, "HTTP_PROXY=" + proxy, "ALL_PROXY=" + proxy}
}

func (s *Server) resolveData(ctx context.Context, tool hosttools.Tool, info *ToolVersion, proxy string) (*hosttools.InstallPin, error) {
	info.Source = "https://github.com/" + tool.Repo
	cmd := exec.CommandContext(ctx, "git", "-c", "credential.helper=", "ls-remote", info.Source+".git", "HEAD")
	cmd.Env = append(os.Environ(), gitEnvironment(proxy)...)
	var output limitedBuffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("无法查询官方字典版本，请检查安装代理后重试")
	}
	commit, err := parseRemoteHead(output.String())
	if err != nil {
		return nil, err
	}
	info.LatestVersion = commit
	return &hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo, Version: commit}, nil
}

func validateDataTree(ctx context.Context, dir string) error {
	for _, name := range []string{"README.md", "Discovery"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("字典内容不完整：缺少%s", name)
		}
	}
	return filepath.WalkDir(dir, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("字典包含不支持的符号链接")
		}
		return nil
	})
}

// A data snapshot never executes repository scripts. Stage and verify its exact
// commit before replacing the old directory; failures leave the old data intact.
func (s *Server) runData(ctx context.Context, j *Job, tool hosttools.Tool, proxy string) (int, error) {
	return s.runDataSource(ctx, j, tool, proxy, "https://github.com/"+tool.Repo+".git")
}

// source is fixed by runData; the split also permits an offline repository fixture.
func (s *Server) runDataSource(ctx context.Context, j *Job, tool hosttools.Tool, proxy, source string) (int, error) {
	if j.installPin == nil || !gitCommit.MatchString(j.installPin.Version) || j.installPin.Repository != tool.Repo {
		return 1, fmt.Errorf("字典版本计划无效，请重新检测")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	parent := filepath.Join(filepath.Dir(j.installBinDir()), "share")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return 1, err
	}
	stage, err := os.MkdirTemp(parent, ".seclists-")
	if err != nil {
		return 1, err
	}
	defer os.RemoveAll(stage)
	env := gitEnvironment(proxy)
	steps := [][]string{
		{"init", "-q", stage},
		{"-C", stage, "remote", "add", "origin", source},
		{"-C", stage, "-c", "credential.helper=", "fetch", "--depth=1", "--no-recurse-submodules", "origin", j.installPin.Version},
		{"-C", stage, "-c", "core.hooksPath=" + filepath.Join(stage, ".disabled-hooks"), "-c", "core.autocrlf=false", "-c", "submodule.recurse=false", "checkout", "--detach", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if code, err := s.execLogged(ctx, j, "git", args, env); err != nil {
			return code, err
		}
	}
	cmd := exec.CommandContext(ctx, "git", "-C", stage, "rev-parse", "HEAD")
	data, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(data)) != j.installPin.Version {
		return 1, fmt.Errorf("字典实际提交与检测版本不一致")
	}
	if err := validateDataTree(ctx, stage); err != nil {
		return 1, err
	}
	if err := os.WriteFile(filepath.Join(stage, ".gondar-version"), []byte(j.installPin.Version+"\n"), 0o644); err != nil {
		return 1, err
	}
	target := filepath.Join(parent, "seclists")
	backup := target + ".previous"
	if _, err := os.Stat(backup); err == nil {
		return 1, fmt.Errorf("存在上次字典备份，请检查%s后重试", backup)
	}
	old := false
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return 1, err
		}
		old = true
	}
	if err := os.Rename(stage, target); err != nil {
		if old {
			_ = os.Rename(backup, target)
		}
		return 1, err
	}
	if old {
		_ = os.RemoveAll(backup)
	}
	j.append("[ok] 字典路径：" + target + "\n")
	return 0, nil
}
