package hostd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gondar/hosttools"
)

type versionTransport func(*http.Request) (*http.Response, error)

func (f versionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fakeResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func newVersionTestServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(context.Background(), log.New(io.Discard, "", 0))
	s.ToolLockPath = filepath.Join(t.TempDir(), "no-lock.json")
	s.StateDir = filepath.Join(t.TempDir(), "receipts")
	s.InstallDir = t.TempDir()
	return s
}

func TestVersionDiscoveryReadOnlyCacheAndFixedSource(t *testing.T) {
	s := newVersionTestServer(t)
	tool, _ := hosttools.Lookup("nuclei")
	var reads atomic.Int32
	digest := strings.Repeat("a", 64)
	s.versionHTTP = &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		reads.Add(1)
		if r.Method != "GET" || r.URL.String() != GitHubAPITagURL(tool) {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		return fakeResponse(fmt.Sprintf(`{"tag_name":"v7.8.9","assets":[{"name":%q,"size":100,"digest":"sha256:%s","browser_download_url":"https://attacker.invalid/evil.zip"}]}`, filepath.Base(GitHubAssetURL(tool, "v7.8.9")), digest)), nil
	})}
	info, err := s.Versions(context.Background(), tool.ID, "")
	if err != nil || info.LatestVersion != "v7.8.9" || info.SHA256 != digest {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	if len(s.List()) != 0 {
		t.Fatal("GET created an install job")
	}
	if _, err := os.Stat(s.StateDir); !os.IsNotExist(err) {
		t.Fatal("GET wrote receipt state")
	}
	second, err := s.Versions(context.Background(), tool.ID, "")
	if err != nil || !second.Cached || reads.Load() != 1 {
		t.Fatalf("cache not used: %+v reads=%d err=%v", second, reads.Load(), err)
	}
	if runtime.GOOS == "linux" && (!info.Installable || info.ResolutionID == "") {
		t.Fatal("verified Linux release did not create a plan")
	}
	if _, err = s.Versions(context.Background(), "../../etc/passwd", ""); err == nil {
		t.Fatal("unknown tool accepted")
	}
	proxy := "http://127.0.0.1:10808"
	if info, err = s.Versions(t.Context(), tool.ID, proxy); err != nil || info.Cached || reads.Load() != 2 {
		t.Fatalf("proxy change reused stale cache: %+v reads=%d err=%v", info, reads.Load(), err)
	}
	request := httptest.NewRequest("GET", "/versions/nuclei?refresh=1&repo=evil", nil)
	request.Header.Set("X-Gondar-Install-Proxy", proxy)
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != 200 || reads.Load() != 3 {
		t.Fatalf("manual refresh reused stale cache: status=%d reads=%d", response.Code, reads.Load())
	}
}

func TestGitHubResolutionRejectsMissingDigestAndUnsafeRelease(t *testing.T) {
	tool, _ := hosttools.Lookup("nuclei")
	asset := filepath.Base(GitHubAssetURL(tool, "v1.2.3"))
	for _, tc := range []struct{ name, body string }{
		{"unqualified digest", fmt.Sprintf(`{"tag_name":"v1.2.3","assets":[{"name":%q,"size":1,"digest":%q}]}`, asset, strings.Repeat("a", 64))},
		{"missing digest", fmt.Sprintf(`{"tag_name":"v1.2.3","assets":[{"name":%q,"size":1}]}`, asset)},
		{"prerelease", fmt.Sprintf(`{"tag_name":"v1.2.3","prerelease":true,"assets":[{"name":%q,"size":1,"digest":"sha256:%s"}]}`, asset, strings.Repeat("a", 64))},
		{"draft", `{"tag_name":"v1.2.3","draft":true}`},
		{"path tag", `{"tag_name":"../v1.2.3"}`},
		{"wrong platform", `{"tag_name":"v1.2.3","assets":[{"name":"nuclei_windows.zip","size":1}]}`},
		{"too large archive", fmt.Sprintf(`{"tag_name":"v1.2.3","assets":[{"name":%q,"size":134217729,"digest":"sha256:%s"}]}`, asset, strings.Repeat("a", 64))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: versionTransport(func(*http.Request) (*http.Response, error) { return fakeResponse(tc.body), nil })}
			info := ToolVersion{}
			if pin, err := resolveGitHub(context.Background(), client, tool, &info); err == nil || pin != nil {
				t.Fatalf("unsafe release accepted: %+v %v", pin, err)
			}
		})
	}
}

func TestGitHubChecksumFallbackBindsExactAssetAndTag(t *testing.T) {
	tool, _ := hosttools.Lookup("nuclei")
	asset := filepath.Base(GitHubAssetURL(tool, "v1.2.3"))
	digest := strings.Repeat("b", 64)
	var reads []string
	client := &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		reads = append(reads, r.URL.String())
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			return fakeResponse(fmt.Sprintf(`{"tag_name":"v1.2.3","assets":[{"name":%q,"size":10},{"name":"nuclei_checksums.txt","size":500}]}`, asset)), nil
		}
		if r.URL.String() != "https://github.com/projectdiscovery/nuclei/releases/download/v1.2.3/nuclei_checksums.txt" {
			t.Errorf("checksum origin/tag drift: %s", r.URL)
		}
		return fakeResponse(digest + "  " + asset + "\n"), nil
	})}
	info := ToolVersion{}
	pin, err := resolveGitHub(context.Background(), client, tool, &info)
	if err != nil || pin.SHA256 != digest || len(reads) != 2 {
		t.Fatalf("pin=%+v reads=%v err=%v", pin, reads, err)
	}
	if _, ok := checksumFor([]byte(digest+"  evil/"+asset), asset); ok {
		t.Fatal("basename-only checksum accepted")
	}
	if _, ok := checksumFor([]byte(digest+"  "+asset+"\n"+strings.Repeat("c", 64)+"  "+asset), asset); ok {
		t.Fatal("ambiguous checksum accepted")
	}
}

func TestVersionMetadataBoundedAndCancelled(t *testing.T) {
	client := &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) { return fakeResponse(strings.Repeat("x", 1025)), nil })}
	if _, err := metadata(context.Background(), client, "https://api.github.com/", 1024); err == nil {
		t.Fatal("oversized response accepted")
	}
	s := newVersionTestServer(t)
	for i := 0; i < cap(s.versions.sem); i++ {
		s.versions.sem <- struct{}{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Versions(ctx, "nuclei", ""); err == nil {
		t.Fatal("cancelled queued check did not stop")
	}
	if len(s.List()) != 0 {
		t.Fatal("cancel created a job")
	}
}

func TestResolvedPlanIdentityExpiryAndSingleUse(t *testing.T) {
	s := newVersionTestServer(t)
	base := installPlan{Pin: hosttools.InstallPin{ToolID: "nuclei", Kind: hosttools.KindGitHub, Repository: "projectdiscovery/nuclei", Version: "v1.2.3", SHA256: strings.Repeat("a", 64)}, Platform: runtime.GOOS + "/" + runtime.GOARCH, Expires: time.Now().Add(time.Minute)}
	s.versions.plans["valid"] = base
	if _, err := s.takeResolution("valid", "httpx"); err == nil {
		t.Fatal("cross-tool plan accepted")
	}
	wrongPlatform := base
	wrongPlatform.Platform = "other/arch"
	s.versions.plans["platform"] = wrongPlatform
	if _, err := s.takeResolution("platform", "nuclei"); err == nil {
		t.Fatal("cross-platform plan accepted")
	}
	expired := base
	expired.Expires = time.Now().Add(-time.Second)
	s.versions.plans["expired"] = expired
	if _, err := s.takeResolution("expired", "nuclei"); err == nil {
		t.Fatal("expired plan accepted")
	}
	if _, err := s.takeResolution("valid", "nuclei"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.takeResolution("valid", "nuclei"); err == nil {
		t.Fatal("consumed plan reused")
	}
}

func TestResolvedArchiveVerifiedBeforeExtractionAndReceipt(t *testing.T) {
	s := newVersionTestServer(t)
	tool, _ := hosttools.Lookup("nuclei")
	var blob bytes.Buffer
	zw := zip.NewWriter(&blob)
	entry, _ := zw.Create("nuclei")
	_, _ = entry.Write([]byte("synthetic bytes; never executed"))
	_ = zw.Close()
	sum := sha256.Sum256(blob.Bytes())
	pin := hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo, Version: "v1.2.3", SHA256: fmt.Sprintf("%x", sum)}
	s.versionHTTP = &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != GitHubAssetURL(tool, pin.Version) {
			t.Errorf("download not pinned: %s", r.URL)
		}
		return fakeResponse(blob.String()), nil
	})}
	j := &Job{ToolID: tool.ID, installPin: &pin, binDir: s.InstallDir}
	pin.SHA256 = strings.Repeat("0", 64)
	if _, err := s.runGitHub(context.Background(), j, tool, ""); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("unverified archive extracted: %v", err)
	}
	if entries, _ := os.ReadDir(s.InstallDir); len(entries) != 0 {
		t.Fatal("checksum mismatch wrote files")
	}
	pin.SHA256 = fmt.Sprintf("%x", sum)
	if _, err := s.runGitHub(context.Background(), j, tool, ""); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.InstallDir, "nuclei"))
	if err != nil || string(raw) != "synthetic bytes; never executed" {
		t.Fatalf("wrong installed payload: %q %v", raw, err)
	}
	if err := s.saveReceipt(j); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.readReceipt("nuclei")
	if err != nil || receipt.Version != pin.Version || receipt.SHA256 != pin.SHA256 {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

func TestVersionHTTPDoesNotInstallOrAcceptSourceOverride(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	s := newVersionTestServer(t)
	s.versionHTTP = &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "pypi.org" {
			t.Errorf("not fixed PyPI: %s", r.URL)
		}
		return fakeResponse(`{"info":{"version":"1.2.3"}}`), nil
	})}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/versions/wafw00f?repo=evil&url=https://attacker.invalid", nil))
	var info ToolVersion
	_ = json.Unmarshal(w.Body.Bytes(), &info)
	wantPlan := runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	if w.Code != 200 || info.LatestVersion != "1.2.3" || info.Installable != wantPlan || (info.ResolutionID != "") != wantPlan || len(s.List()) != 0 {
		t.Fatalf("unsafe PyPI observation: %d %+v", w.Code, info)
	}
	if wantPlan {
		plan, err := s.takeResolution(info.ResolutionID, "wafw00f")
		if err != nil || plan.Pin.Package != "wafw00f" || plan.Pin.Version != "1.2.3" || plan.Source != "https://pypi.org/project/wafw00f/" {
			t.Fatalf("wrong official PyPI plan: %+v %v", plan, err)
		}
	}
}

func TestOfficialSourceRedirectPolicy(t *testing.T) {
	for _, address := range []string{"http://github.com/x", "https://attacker.invalid/x", "https://github.com:444/x", "https://user:pass@github.com/x"} {
		u, _ := url.Parse(address)
		if officialRedirect(&http.Request{URL: u}, nil) == nil {
			t.Fatalf("accepted redirect %s", address)
		}
	}
	u, _ := url.Parse("https://release-assets.githubusercontent.com/fixed-object")
	if err := officialRedirect(&http.Request{URL: u}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestResolvedJobKeepsVersionAndQueuedCancellation(t *testing.T) {
	t.Setenv("PATH", "")
	s := newVersionTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.sem <- struct{}{}
	s.versions.plans["fixed"] = installPlan{Pin: hosttools.InstallPin{ToolID: "nuclei", Kind: hosttools.KindGitHub, Repository: "projectdiscovery/nuclei", Version: "v1.2.3", SHA256: strings.Repeat("a", 64)}, Platform: runtime.GOOS + "/" + runtime.GOARCH, Expires: time.Now().Add(time.Minute)}
	j, err := s.start(ctx, "nuclei", "", "fixed")
	if err != nil || j.Version != "v1.2.3" {
		t.Fatalf("job=%+v err=%v", j, err)
	}
	if _, err := s.start(ctx, "nuclei", "", "unknown-plan"); err == nil {
		t.Fatal("forged plan fell back to a lock/latest")
	}
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		j.mu.Lock()
		state := j.State
		end := j.EndedAt
		j.mu.Unlock()
		if state == StateCancelled {
			if end.IsZero() {
				t.Fatal("queued cancellation omitted end time")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("queued job did not cancel while semaphore was occupied")
}

func TestAPTCandidatesBindEveryRecipePackage(t *testing.T) {
	tool, _ := hosttools.Lookup("libreoffice")
	valid := "libreoffice-writer=4:7.4.7-1+deb12u8, libreoffice-calc=4:7.4.7-1+deb12u8, libreoffice-impress=4:7.4.7-1+deb12u8"
	packages, err := aptPlanPackages(tool, valid)
	if err != nil || len(packages) != 3 {
		t.Fatalf("packages=%v err=%v", packages, err)
	}
	for _, invalid := range []string{
		"libreoffice-writer=1.0.0",
		strings.Replace(valid, "libreoffice-calc=", "attacker-package=", 1),
		strings.Replace(valid, "4:7.4.7-1+deb12u8", "--allow-unauthenticated", 1),
		strings.Replace(valid, "4:7.4.7-1+deb12u8", "latest", 1),
	} {
		if _, err := aptPlanPackages(tool, invalid); err == nil {
			t.Fatalf("accepted unsafe candidate: %s", invalid)
		}
	}
}

func TestInstallerProxyCredentialsNotStoredInJobLog(t *testing.T) {
	j := &Job{redactions: []string{"http://operator:synthetic-password@proxy.invalid:8080", "http://operator:***@proxy.invalid:8080", "synthetic-password", "***"}}
	j.append("$ apt-get -o Acquire::http::Proxy=http://operator:synthetic-password@proxy.invalid:8080 install nmap\n")
	if strings.Contains(j.Log, "synthetic-password") || !strings.Contains(j.Log, "***") {
		t.Fatal("proxy credential retained in install log")
	}
}

func TestResolvedExtractionRejectsMissingTarMemberAndOversizedZip(t *testing.T) {
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "different-file", Mode: 0o755, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	tool, _ := hosttools.Lookup("gitleaks")
	j := &Job{binDir: t.TempDir()}
	if _, err := installFromArchive(j, archive.Bytes(), tool); err == nil {
		t.Fatal("tar without target was installed as raw gzip bytes")
	}
	if files, _ := os.ReadDir(j.binDir); len(files) != 0 {
		t.Fatal("missing target wrote a binary")
	}
	archive.Reset()
	gz = gzip.NewWriter(&archive)
	_, _ = gz.Write([]byte("synthetic chisel bytes"))
	_ = gz.Close()
	chisel, _ := hosttools.Lookup("chisel")
	if _, err := installFromArchive(j, archive.Bytes(), chisel); err != nil {
		t.Fatalf("single-file gzip regressed: %v", err)
	}
	var zipped bytes.Buffer
	zw := zip.NewWriter(&zipped)
	entry, _ := zw.Create("nuclei")
	_, _ = entry.Write([]byte("x"))
	_ = zw.Close()
	zr, _ := zip.NewReader(bytes.NewReader(zipped.Bytes()), int64(zipped.Len()))
	zr.File[0].UncompressedSize64 = (256 << 20) + 1
	nuclei, _ := hosttools.Lookup("nuclei")
	if _, err := installFromZip(j, zr, nuclei); err == nil {
		t.Fatal("oversized member accepted")
	}
	if _, err := os.Stat(filepath.Join(j.binDir, "nuclei")); !os.IsNotExist(err) {
		t.Fatal("oversized member wrote a binary")
	}
}

func TestResolvedArchiveRetriesOnlyTransientFailuresAtSameURL(t *testing.T) {
	address := "https://github.com/projectdiscovery/nuclei/releases/download/v1.2.3/nuclei_1.2.3_linux_amd64.zip"
	var attempts atomic.Int32
	client := &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != address {
			t.Errorf("retry changed fixed version: %s", r.URL)
		}
		if attempts.Add(1) == 1 {
			response := fakeResponse("temporary")
			response.StatusCode = 503
			return response, nil
		}
		return fakeResponse("synthetic archive bytes"), nil
	})}
	blob, err := fetchToolArchive(context.Background(), client, address)
	if err != nil || string(blob) != "synthetic archive bytes" || attempts.Load() != 2 {
		t.Fatalf("attempts=%d blob=%q err=%v", attempts.Load(), blob, err)
	}
	attempts.Store(0)
	client.Transport = versionTransport(func(*http.Request) (*http.Response, error) {
		attempts.Add(1)
		response := fakeResponse("not found")
		response.StatusCode = 404
		return response, nil
	})
	if _, err := fetchToolArchive(context.Background(), client, address); err == nil || attempts.Load() != 1 {
		t.Fatal("404 retried or accepted")
	}
}

func TestResolvedInstallJobDoesNotFollowLaterLatest(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("installation plan platform is Linux; exercised in isolated Linux container")
	}
	t.Setenv("PATH", "")
	s := newVersionTestServer(t)
	tool, _ := hosttools.Lookup("nuclei")
	var blob bytes.Buffer
	zw := zip.NewWriter(&blob)
	entry, _ := zw.Create("nuclei")
	_, _ = entry.Write([]byte("synthetic fixture bytes, never executed"))
	_ = zw.Close()
	digest := fmt.Sprintf("%x", sha256.Sum256(blob.Bytes()))
	var metadataReads atomic.Int32
	s.versionHTTP = &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			metadataReads.Add(1)
			return fakeResponse(fmt.Sprintf(`{"tag_name":"v1.2.3","assets":[{"name":%q,"size":%d,"digest":"sha256:%s"}]}`, filepath.Base(GitHubAssetURL(tool, "v1.2.3")), blob.Len(), digest)), nil
		}
		if r.URL.String() != GitHubAssetURL(tool, "v1.2.3") {
			t.Errorf("install drifted from detected version: %s", r.URL)
		}
		return fakeResponse(blob.String()), nil
	})}
	info, err := s.Versions(context.Background(), tool.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.start(context.Background(), tool.ID, "", info.ResolutionID)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j.mu.Lock()
		state := j.State
		j.mu.Unlock()
		if state == StateDone {
			if j.Version != "v1.2.3" || metadataReads.Load() != 1 {
				t.Fatalf("version drift or latest re-resolved: version=%s reads=%d", j.Version, metadataReads.Load())
			}
			receipt, err := s.readReceipt(tool.ID)
			if err != nil || receipt.SHA256 != digest {
				t.Fatalf("receipt=%+v err=%v", receipt, err)
			}
			return
		}
		if state == StateFailed {
			j.mu.Lock()
			message := j.Err
			j.mu.Unlock()
			t.Fatal(message)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("verified fixture install did not finish")
}

func TestLegacyToolLockCannotOverrideOfficialVersion(t *testing.T) {
	s := newVersionTestServer(t)
	tool, _ := hosttools.Lookup("nuclei")
	lock := hosttools.InstallLock{Format: 1, OS: "linux", Arch: runtime.GOARCH, Pins: []hosttools.InstallPin{{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo, Version: "v1.0.0", SHA256: strings.Repeat("d", 64)}}}
	before, _ := json.Marshal(lock)
	if err := os.WriteFile(s.ToolLockPath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	s.versionHTTP = &http.Client{Transport: versionTransport(func(*http.Request) (*http.Response, error) {
		return fakeResponse(fmt.Sprintf(`{"tag_name":"v9.9.9","assets":[{"name":%q,"size":1,"digest":"sha256:%s"}]}`, filepath.Base(GitHubAssetURL(tool, "v9.9.9")), strings.Repeat("a", 64))), nil
	})}
	info, err := s.Versions(context.Background(), tool.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(s.ToolLockPath)
	if !bytes.Equal(before, after) {
		t.Fatal("GET changed an administrator lock")
	}
	if info.LockedVersion != "" || info.LockedAvailable || info.LatestVersion != "v9.9.9" {
		t.Fatalf("legacy lock affected official version: %+v", info)
	}
}
