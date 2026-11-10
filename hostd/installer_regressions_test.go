package hostd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gondar/hosttools"
)

func elfFixture() []byte {
	data := make([]byte, 64)
	copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(data[16:], 2)
	machine := uint16(62)
	if runtime.GOARCH == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(data[18:], machine)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint16(data[52:], 64)
	return data
}

func TestRawScriptAssetIsExplicitAndValidated(t *testing.T) {
	tool, _ := hosttools.Lookup("linpeas")
	tool.BinInZip = tool.Bin
	dir := t.TempDir()
	j := &Job{binDir: dir, installPin: &hosttools.InstallPin{Version: "20261107-fixture"}}
	for _, bad := range [][]byte{[]byte("<html>download failed</html>"), elfFixture()} {
		if _, err := installFromArchive(j, bad, tool); err == nil {
			t.Fatal("a non-script body was accepted as the shell asset")
		}
	}
	data := []byte("#!/bin/sh\nexit 0\n")
	if _, err := installFromArchive(j, data, tool); err != nil {
		t.Fatal(err)
	}
	observed, err := os.ReadFile(filepath.Join(dir, tool.Bin))
	if err != nil || !bytes.Equal(data, observed) {
		t.Fatalf("raw asset changed: %v", err)
	}
	var record hosttools.InstalledRecord
	raw, err := os.ReadFile(filepath.Join(dir, tool.Bin) + ".gondar-version.json")
	if err != nil || json.Unmarshal(raw, &record) != nil || record.ToolID != tool.ID || record.Version != "20261107-fixture" {
		t.Fatalf("missing observed file version: %s %v", raw, err)
	}
	other, _ := hosttools.Lookup("nuclei")
	if _, err := installFromArchive(j, data, other); err == nil {
		t.Fatal("raw installation silently enabled for compressed recipe")
	}
}

func TestSourceBuildELFMustMatchCurrentPlatform(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux executable validation")
	}
	if err := validateRawELF(elfFixture()); err != nil {
		t.Fatal(err)
	}
	if err := validateRawELF([]byte("<html>failed</html>")); err == nil {
		t.Fatal("invalid binary accepted")
	}
	wrong := elfFixture()
	binary.LittleEndian.PutUint16(wrong[18:], 3)
	if err := validateRawELF(wrong); err == nil {
		t.Fatal("wrong architecture accepted")
	}
}

func TestLigoloArchiveProxyBecomesStableCommand(t *testing.T) {
	tool, _ := hosttools.Lookup("ligolo-ng")
	j := &Job{binDir: t.TempDir()}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	data := []byte("proxy fixture")
	if err := tw.WriteHeader(&tar.Header{Name: "proxy", Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	if _, err := installFromArchive(j, buf.Bytes(), tool); err != nil {
		t.Fatal(err)
	}
	observed, err := os.ReadFile(filepath.Join(j.binDir, "ligolo-ng"))
	if err != nil || !bytes.Equal(data, observed) {
		t.Fatalf("proxy not installed under expected CLI: %v", err)
	}
}

func TestSourceResolutionPrefersPeeledReleaseCommit(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	raw := a + "\trefs/tags/v1.2.1\n" + b + "\trefs/tags/v1.2.1^{}\n"
	got, err := parseSourceRef(raw, "refs/tags/v1.2.1")
	if err != nil || got != b {
		t.Fatalf("annotated tag object selected instead of source commit: %s %v", got, err)
	}
	if _, err := parseSourceRef(a+"\trefs/heads/evil", "HEAD"); err == nil {
		t.Fatal("unrelated ref accepted")
	}
}

func TestGemMetadataBindsOfficialPackageAndDigest(t *testing.T) {
	tool, _ := hosttools.Lookup("evil-winrm")
	for _, tc := range []struct {
		name, digest string
		valid        bool
	}{{"evil-winrm", strings.Repeat("a", 64), true}, {"lookalike", strings.Repeat("a", 64), false}, {"evil-winrm", "", false}} {
		client := &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://rubygems.org/api/v1/gems/evil-winrm.json" {
				t.Fatalf("unexpected gem origin: %s", r.URL)
			}
			return fakeResponse(fmt.Sprintf(`{"name":%q,"version":"4.1","sha":%q}`, tc.name, tc.digest)), nil
		})}
		info := ToolVersion{}
		pin, err := resolveGem(context.Background(), client, tool, &info)
		if (err == nil) != tc.valid || (tc.valid && (pin.Package != "evil-winrm" || pin.Version != "4.1")) {
			t.Fatalf("invalid gem metadata result: %+v %v", pin, err)
		}
	}
}
