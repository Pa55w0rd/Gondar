package hostd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gondar/hosttools"
)

// The Git object ID is the integrity anchor for source installations. No
// checksum is invented for an old release binary which lacks a publisher hash.
func (s *Server) resolveSource(ctx context.Context, client *http.Client, tool hosttools.Tool, info *ToolVersion, proxy string) (*hosttools.InstallPin, error) {
	info.Source = "https://github.com/" + tool.Repo
	ref, version := "HEAD", ""
	if tool.ID == "pspy64" {
		raw, err := metadata(ctx, client, GitHubAPITagURL(tool), 2<<20)
		var release struct {
			Tag        string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		}
		if err != nil {
			version, err = latestOfficialTag(ctx, client, tool.Repo)
			if err != nil {
				return nil, err
			}
		} else {
			if json.Unmarshal(raw, &release) != nil || release.Draft || release.Prerelease || !exactVersion.MatchString(release.Tag) || strings.Contains(release.Tag, "..") || strings.EqualFold(release.Tag, "latest") {
				return nil, fmt.Errorf("官方源码版本不是稳定 Release")
			}
			version = release.Tag
		}
		ref = "refs/tags/" + version
	} else if tool.ID != "responder" {
		return nil, fmt.Errorf("没有此工具的固定源码配方")
	}
	args := []string{"-c", "credential.helper=", "ls-remote", info.Source + ".git", ref}
	if ref != "HEAD" {
		args = append(args, ref+"^{}")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), gitEnvironment(proxy)...)
	var out limitedBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("无法获取官方源码的确切提交，请检查安装代理")
	}
	commit, err := parseSourceRef(out.String(), ref)
	if err != nil {
		return nil, err
	}
	if version == "" {
		version = commit
	}
	info.LatestVersion = version
	return &hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo, Version: version, Commit: commit}, nil
}

func parseSourceRef(raw, ref string) (string, error) {
	commit := ""
	for _, line := range strings.Split(raw, "\n") {
		p := strings.Fields(line)
		if len(p) != 2 || !gitCommit.MatchString(p[0]) {
			continue
		}
		if p[1] == ref+"^{}" {
			return p[0], nil
		}
		if p[1] == ref {
			commit = p[0]
		}
	}
	if commit == "" {
		return "", fmt.Errorf("官方源码没有返回确切提交")
	}
	return commit, nil
}

func latestOfficialTag(ctx context.Context, client *http.Client, repo string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, "https://github.com/"+repo+"/releases/latest", nil)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("无法读取官方发行版本")
	}
	resp.Body.Close()
	prefix := "/" + repo + "/releases/tag/"
	if resp.StatusCode != 200 || resp.Request == nil || resp.Request.URL.Scheme != "https" || resp.Request.URL.Host != "github.com" || !strings.HasPrefix(resp.Request.URL.Path, prefix) {
		return "", fmt.Errorf("官方发行页没有返回确切版本")
	}
	tag := strings.TrimPrefix(resp.Request.URL.Path, prefix)
	if !exactVersion.MatchString(tag) || strings.Contains(tag, "..") || strings.EqualFold(tag, "latest") {
		return "", fmt.Errorf("官方发行版本无效")
	}
	return tag, nil
}

func shellArgument(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func (s *Server) bootstrap(ctx context.Context, j *Job, proxy, packages string) (int, error) {
	return s.runApt(ctx, j, hosttools.Tool{Name: "工具构建依赖", Apt: packages}, proxy)
}

func (s *Server) runSource(ctx context.Context, j *Job, tool hosttools.Tool, proxy string) (int, error) {
	if j.installPin == nil || j.installPin.Kind != hosttools.KindSource || j.installPin.Repository != tool.Repo || !gitCommit.MatchString(j.installPin.Commit) || (tool.ID != "responder" && tool.ID != "pspy64") {
		return 1, fmt.Errorf("源码版本计划无效，请重新检测")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	parent := filepath.Join(filepath.Dir(j.installBinDir()), "share", tool.ID)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return 1, err
	}
	target := filepath.Join(parent, j.installPin.Commit)
	if _, err := os.Stat(target); os.IsNotExist(err) {
		stage, err := os.MkdirTemp(parent, ".source-")
		if err != nil {
			return 1, err
		}
		defer os.RemoveAll(stage)
		for _, args := range [][]string{
			{"init", "-q", stage},
			{"-C", stage, "remote", "add", "origin", "https://github.com/" + tool.Repo + ".git"},
			{"-C", stage, "-c", "credential.helper=", "fetch", "--depth=1", "--no-recurse-submodules", "origin", j.installPin.Commit},
			{"-C", stage, "-c", "core.hooksPath=" + filepath.Join(stage, ".disabled-hooks"), "-c", "core.autocrlf=false", "-c", "submodule.recurse=false", "checkout", "--detach", "FETCH_HEAD"},
		} {
			if code, err := s.execLogged(ctx, j, "git", args, gitEnvironment(proxy)); err != nil {
				return code, err
			}
		}
		if err := os.Rename(stage, target); err != nil {
			return 1, err
		}
	}
	check := exec.CommandContext(ctx, "git", "-C", target, "rev-parse", "HEAD")
	actual, err := check.Output()
	if err != nil || strings.TrimSpace(string(actual)) != j.installPin.Commit {
		return 1, fmt.Errorf("源码提交与版本计划不一致")
	}
	if tool.ID == "pspy64" {
		if _, err := exec.LookPath("go"); err != nil {
			if code, err := s.bootstrap(ctx, j, proxy, "golang-go"); err != nil {
				return code, err
			}
		}
		filename := filepath.Join(target, ".gondar-pspy64")
		flags := "-s -w -X main.version=" + j.installPin.Version + " -X main.commit=" + j.installPin.Commit
		env := []string{"CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + runtime.GOARCH, "GOTOOLCHAIN=local", "GOWORK=off", "GOENV=off", "GOFLAGS=", "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org", "GOPRIVATE=", "GONOSUMDB=", "GONOPROXY=", "GOINSECURE=", "HTTPS_PROXY=" + proxy, "HTTP_PROXY=" + proxy}
		if code, err := s.execLoggedInDir(ctx, j, target, "go", []string{"build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags", flags, "-o", filename, "."}, env); err != nil {
			return code, err
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			return 1, err
		}
		if err = validateRawELF(data); err != nil {
			return 1, err
		}
		return installBinary(j, tool, data)
	}
	entry := filepath.Join(target, "Responder.py")
	if st, err := os.Lstat(entry); err != nil || !st.Mode().IsRegular() {
		return 1, fmt.Errorf("官方源码缺少 Responder.py")
	}
	if _, err := exec.LookPath("cc"); err != nil {
		if code, err := s.bootstrap(ctx, j, proxy, "gcc libc6-dev"); err != nil {
			return code, err
		}
	}
	venv := filepath.Join(target, ".venv")
	python := filepath.Join(venv, "bin", "python3")
	if _, err := os.Stat(python); err != nil {
		if code, err := s.execLogged(ctx, j, "python3", []string{"-m", "venv", venv}, nil); err != nil {
			return code, err
		}
	}
	args := []string{"-m", "pip", "install", "--disable-pip-version-check", "--index-url", "https://pypi.org/simple", "aioquic", "netifaces>=0.10.4", "pyasn1"}
	if proxy != "" {
		args = append(args, "--proxy", proxy)
	}
	if code, err := s.execLogged(ctx, j, python, args, []string{"PIP_CONFIG_FILE=/dev/null", "PIP_INDEX_URL=https://pypi.org/simple", "PIP_EXTRA_INDEX_URL="}); err != nil {
		return code, err
	}
	wrapper := "#!/bin/sh\ncd " + shellArgument(target) + " || exit 1\nexec " + shellArgument(python) + " " + shellArgument(entry) + " \"$@\"\n"
	return installBinary(j, tool, []byte(wrapper))
}
