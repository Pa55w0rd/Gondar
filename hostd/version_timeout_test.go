package hostd

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gondar/hosttools"
)

func TestMetadataTimeoutPreservesInstalledStateAndOffersNoInstall(t *testing.T) {
	s := newVersionTestServer(t)
	t.Setenv("PATH", s.InstallDir)
	tool, _ := hosttools.Lookup("linpeas")
	name := tool.Bin
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	data := []byte("synthetic file; never execute")
	if err := os.WriteFile(filepath.Join(s.InstallDir, name), data, 0o755); err != nil {
		t.Fatal(err)
	}
	j := &Job{binDir: s.InstallDir, installPin: &hosttools.InstallPin{Version: "20261107-fixture"}}
	if err := saveInstalledRecord(j, tool, data); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if err := os.Rename(filepath.Join(s.InstallDir, tool.Bin)+".gondar-version.json", filepath.Join(s.InstallDir, name)+".gondar-version.json"); err != nil {
			t.Fatal(err)
		}
	}
	s.versionHTTP = &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	got, err := s.Versions(ctx, tool.ID, "")
	if err != nil || !got.Available || got.InstalledVersion != j.installPin.Version || got.LatestVersion != "" || got.Installable || got.UpdateAvailable || got.ResolutionID != "" || !strings.Contains(got.Reason, "超时") {
		t.Fatalf("timeout erased local state or offered an install: %+v %v", got, err)
	}
}
