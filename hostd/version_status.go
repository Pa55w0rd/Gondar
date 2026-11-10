package hostd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
	"gondar/hosttools"
)

var baseVersionPattern = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

func baseVersion(v string) string { return baseVersionPattern.FindString(v) }

var numericVersion = regexp.MustCompile(`\d+`)

// Stable releases are compared numerically; equal Debian upstream versions
// use the package manager's full version comparison, including security suffixes.
func newerVersion(current, latest string) bool {
	if current == "" || latest == "" || strings.EqualFold(strings.TrimPrefix(current, "v"), strings.TrimPrefix(latest, "v")) {
		return false
	}
	aVersion, bVersion := "v"+strings.TrimPrefix(current, "v"), "v"+strings.TrimPrefix(latest, "v")
	if semver.IsValid(aVersion) && semver.IsValid(bVersion) {
		return semver.Compare(aVersion, bVersion) < 0
	}
	a, b := numericVersion.FindAllString(current, -1), numericVersion.FindAllString(latest, -1)
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y uint64
		if i < len(a) {
			x, _ = strconv.ParseUint(a[i], 10, 64)
		}
		if i < len(b) {
			y, _ = strconv.ParseUint(b[i], 10, 64)
		}
		if x != y {
			return y > x
		}
	}
	return false
}

func aptVersionNewer(current, latest string) bool {
	parts := strings.Split(latest, ", ")
	if len(parts) > 1 {
		old := map[string]string{}
		for _, item := range strings.Split(current, ", ") {
			key, value, _ := strings.Cut(item, "=")
			old[key] = value
		}
		for _, item := range parts {
			key, value, _ := strings.Cut(item, "=")
			if old[key] != "" && aptVersionNewer(old[key], value) {
				return true
			}
		}
		return false
	}
	if current == "" || latest == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "dpkg", "--compare-versions", strings.TrimPrefix(current, "v"), "lt", strings.TrimPrefix(latest, "v"))
	if err := cmd.Run(); err == nil {
		return true
	} else if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false
	}
	return newerVersion(current, latest)
}

func finishVersionStatus(info *ToolVersion) {
	info.UpdateAvailable = info.Available && info.InstalledVersion != "" && info.LatestVersion != "" && newerVersion(info.InstalledVersion, info.LatestVersion)
	if info.Kind == hosttools.KindApt {
		info.UpdateAvailable = info.Available && aptVersionNewer(info.InstalledVersion, info.LatestVersion)
	}
	if info.Kind == hosttools.KindData || (info.Kind == hosttools.KindSource && info.ToolID == "responder") {
		info.UpdateAvailable = info.Available && info.InstalledVersion != "" && info.LatestVersion != "" && info.InstalledVersion != info.LatestVersion
	}
	info.State = "missing"
	if info.Available {
		info.State = "unknown"
		if info.InstalledVersion != "" && info.LatestVersion != "" {
			info.State = "current"
		}
		if info.UpdateAvailable {
			info.State = "update"
		}
	}
}

type aptRefreshState struct {
	sync.Mutex
	done  chan struct{}
	until time.Time
	err   error
	proxy string
}

// Refresh package metadata once, without installing anything. Share the same
// semaphore as installation jobs so apt operations do not compete for locks.
func (s *Server) ensureAPTMetadata(ctx context.Context, proxy string) error {
	state := &s.versions.apt
	state.Lock()
	if state.proxy == proxy && time.Now().Before(state.until) {
		err := state.err
		state.Unlock()
		return err
	}
	if state.done == nil {
		state.done = make(chan struct{})
		done := state.done
		go func() {
			base, cancel := context.WithTimeout(s.base, 75*time.Second)
			defer cancel()
			var err error
			select {
			case s.sem <- struct{}{}:
				args := aptUpdateArgv(s.pkgMgr, proxy)
				cmd := exec.CommandContext(base, args[0], append(args[1:], "-qq", "-o", "Acquire::Retries=0", "-o", "Acquire::http::Timeout=10", "-o", "Acquire::https::Timeout=10")...)
				cmd.Env = append(os.Environ(), "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive")
				if cmd.Run() != nil {
					err = fmt.Errorf("软件源版本信息刷新失败，请检查安装代理后重试")
				}
				<-s.sem
			case <-base.Done():
				err = fmt.Errorf("软件源版本查询超时，请稍后重试")
			}
			state.Lock()
			state.err = err
			state.proxy = proxy
			state.until = time.Now().Add(30 * time.Minute)
			if err != nil {
				state.until = time.Now().Add(15 * time.Second)
			}
			state.done = nil
			close(done)
			state.Unlock()
		}()
	}
	done := state.done
	state.Unlock()
	select {
	case <-done:
		state.Lock()
		err := state.err
		state.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func aptDisplayVersion(value string) string {
	parts := strings.Split(value, ", ")
	if len(parts) == 1 {
		if _, v, ok := strings.Cut(value, "="); ok {
			return v
		}
	}
	return value
}
