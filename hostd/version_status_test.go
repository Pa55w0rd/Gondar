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
	"runtime"
	"strings"
	"testing"
	"time"

	"gondar/hosttools"
)

func TestVersionActionsRequireMissingOrConfirmedUpdate(t *testing.T) {
	for _, tc := range []struct {
		name, current, latest string
		available, want       bool
	}{
		{"same", "3.11.1", "v3.11.1", true, false}, {"newer", "3.11.1", "v3.12.0", true, true}, {"downgrade", "3.12.0", "v3.11.1", true, false},
		{"unknown current", "", "v3.11.1", true, false}, {"unknown latest", "3.11.1", "", true, false}, {"missing", "", "v3.11.1", false, false},
		{"stable after preview", "1.2.3-rc1", "v1.2.3", true, true}, {"numeric order", "1.10.0", "v1.9.0", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := ToolVersion{Kind: hosttools.KindGitHub, Available: tc.available, InstalledVersion: tc.current, LatestVersion: tc.latest}
			finishVersionStatus(&v)
			if v.UpdateAvailable != tc.want {
				t.Fatalf("status=%+v", v)
			}
		})
	}
}

func TestInstalledUnknownVersionHasNoReinstallPlan(t *testing.T) {
	dir := t.TempDir()
	name := "nuclei"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("not an executable; availability fixture"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	s := newVersionTestServer(t)
	tool, _ := hosttools.Lookup("nuclei")
	s.versionHTTP = &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		return fakeResponse(fmt.Sprintf(`{"tag_name":"v8.9.0","assets":[{"name":%q,"size":20,"digest":"sha256:%s"}]}`, filepath.Base(GitHubAssetURL(tool, "v8.9.0")), strings.Repeat("a", 64))), nil
	})}
	v, err := s.Versions(t.Context(), "nuclei", "")
	if err != nil || !v.Available || v.Installable || v.ResolutionID != "" {
		t.Fatalf("reinstall offered: %+v %v", v, err)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "locked_") {
		t.Fatal("removed lock fields still public")
	}
	s.versions.plans["old-missing-plan"] = installPlan{Pin: hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Version: "v8.9.0", Repository: tool.Repo, SHA256: strings.Repeat("a", 64)}, Platform: runtime.GOOS + "/" + runtime.GOARCH, Expires: time.Now().Add(time.Minute)}
	if job, err := s.start(t.Context(), tool.ID, "", "old-missing-plan"); err == nil || job != nil || len(s.List()) != 0 {
		t.Fatal("stale missing plan reinstalled existing tool")
	}
}

func TestGitHubRateLimitFallbackStillRequiresExactChecksum(t *testing.T) {
	tool, _ := hosttools.Lookup("naabu")
	asset := filepath.Base(GitHubAssetURL(tool, "v2.3.4"))
	digest := strings.Repeat("b", 64)
	for _, wrongRepo := range []bool{false, true} {
		t.Run(fmt.Sprint(wrongRepo), func(t *testing.T) {
			client := &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "api.github.com" {
					resp := fakeResponse("")
					resp.StatusCode = 403
					resp.Header.Set("X-RateLimit-Remaining", "0")
					return resp, nil
				}
				if r.Method == http.MethodHead {
					resp := fakeResponse("")
					copy := r.Clone(r.Context())
					target := "https://github.com/" + tool.Repo + "/releases/tag/v2.3.4"
					if wrongRepo {
						target = "https://github.com/other/project/releases/tag/v2.3.4"
					}
					copy.URL, _ = url.Parse(target)
					resp.Request = copy
					return resp, nil
				}
				if !strings.Contains(r.URL.Path, "/releases/download/v2.3.4/") {
					if r.URL.Path == "/"+tool.Repo+"/releases/expanded_assets/v2.3.4" {
						resp := fakeResponse("")
						resp.StatusCode = 404
						return resp, nil
					}
					t.Fatalf("source drift: %s", r.URL)
				}
				if !strings.HasSuffix(r.URL.Path, "/naabu-checksums.txt") {
					resp := fakeResponse("")
					resp.StatusCode = 404
					return resp, nil
				}
				return fakeResponse(digest + "  " + asset + "\n"), nil
			})}
			info := ToolVersion{}
			pin, err := resolveGitHub(t.Context(), client, tool, &info)
			if wrongRepo {
				if err == nil || pin != nil {
					t.Fatal("cross repository accepted")
				}
				return
			}
			if err != nil || pin.SHA256 != digest || pin.Version != "v2.3.4" {
				t.Fatalf("pin=%+v err=%v", pin, err)
			}
		})
	}
}

func TestPyPIVersionDoesNotDownloadReleaseHistory(t *testing.T) {
	tool, _ := hosttools.Lookup("pymongo")
	client := &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://pypi.org/pypi/pymongo/json" {
			t.Fatalf("unexpected source: %s", r.URL)
		}
		return fakeResponse(`{"info":{"version":"4.15.4"},"releases":"` + strings.Repeat("x", 3<<20) + `"}`), nil
	})}
	info := ToolVersion{}
	pin, err := resolvePyPI(t.Context(), client, tool, &info)
	if err != nil || pin == nil || pin.Version != "4.15.4" || pin.Package != "pymongo" || info.LatestVersion != pin.Version {
		t.Fatalf("large release history hid current version: %+v %v", pin, err)
	}
}

// Explicitly opt in: only official release metadata is fetched, never an
// executable archive. The API failure is synthetic; the fallback reads are real.
func TestGitHubOfficialFallbackOptIn(t *testing.T) {
	if os.Getenv("GONDAR_TEST_OFFICIAL_METADATA") != "1" {
		t.Skip("official network metadata check requires explicit opt-in")
	}
	s := newVersionTestServer(t)
	client := s.metadataClient("")
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 18*time.Second)
	defer cancel()
	tool, _ := hosttools.Lookup("naabu")
	info := ToolVersion{}
	pin, err := githubReleaseFallback(ctx, client, tool, &info, &metadataStatusError{Status: 403, RateLimited: true})
	if err != nil || pin == nil || pin.Repository != tool.Repo || !digestPattern.MatchString(pin.SHA256) || pin.Version != info.LatestVersion {
		t.Fatalf("official fallback failed: pin=%+v err=%v", pin, err)
	}
	t.Logf("官方版本=%s SHA256=%s；没有下载安装包", pin.Version, pin.SHA256)
}

func TestSecListsOfflineInstallPinsDataWithoutExecutingScripts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	repo := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fixture git failed: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", repo)
	os.Mkdir(filepath.Join(repo, "Discovery"), 0o755)
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("fixture"), 0o644)
	os.WriteFile(filepath.Join(repo, "Discovery", "words.txt"), []byte("one\ntwo\n"), 0o644)
	os.WriteFile(filepath.Join(repo, "install.sh"), []byte("exit 99\n"), 0o755)
	git("-C", repo, "add", ".")
	git("-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
	commit := git("-C", repo, "rev-parse", "HEAD")
	if got, err := parseRemoteHead(commit + "\tHEAD\n"); err != nil || got != commit {
		t.Fatal(got, err)
	}
	tool, _ := hosttools.Lookup("seclists")
	s := newVersionTestServer(t)
	bin := filepath.Join(t.TempDir(), "bin")
	j := &Job{ToolID: tool.ID, binDir: bin, installPin: &hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo, Version: commit}}
	if code, err := s.runDataSource(context.Background(), j, tool, "", repo); err != nil || code != 0 {
		t.Fatalf("install failed: %d %v %s", code, err, j.Log)
	}
	dest := filepath.Join(filepath.Dir(bin), "share", "seclists")
	data, err := os.ReadFile(filepath.Join(dest, "Discovery", "words.txt"))
	if err != nil || string(data) != "one\ntwo\n" {
		t.Fatalf("data missing: %q %v", data, err)
	}
	t.Setenv("GONDAR_SECLISTS_DIR", dest)
	current := hosttools.ProbeInstalled(t.Context(), tool)
	if !current.Available || current.Version != commit {
		t.Fatalf("data is not discoverable: %+v", current)
	}
	if _, err := parseRemoteHead("not-a-commit HEAD"); err == nil {
		t.Fatal("invalid remote commit accepted")
	}
	j.installPin.Version = strings.Repeat("a", 40)
	if _, err := s.runDataSource(t.Context(), j, tool, "", repo); err == nil {
		t.Fatal("unknown commit installed")
	}
	if data, err := os.ReadFile(filepath.Join(dest, "Discovery", "words.txt")); err != nil || string(data) != "one\ntwo\n" {
		t.Fatal("failed update modified existing dictionary")
	}
}
