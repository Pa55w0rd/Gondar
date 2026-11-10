package doctor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gondar/hosttools"
	"gondar/selfupdate"
)

func TestDoctorRecognizesImageMagickAliasWithoutExecution(t *testing.T) {
	dir := t.TempDir()
	name := "convert"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	filename := filepath.Join(dir, name)
	if err := os.WriteFile(filename, []byte("synthetic, deliberately not executable content"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	tool, _ := hosttools.Lookup("imagemagick")
	got := inspectTool(t.Context(), tool, false, false)
	if !got.Available || got.Path != filename || got.VersionSource != "未执行本地工具" {
		t.Fatalf("alias missing from diagnostic: %+v", got)
	}
}

func TestDoctorDoesNotCreateFilesOrExposeConnectionSecrets(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "not-created", "data")
	config := filepath.Join(dir, "missing-config.json")
	r := Run(context.Background(), Options{ConfigPath: config, DataDir: data, DSN: "postgres://doctor:synthetic-private-password@127.0.0.1:1/private?sslmode=disable", Tools: []hosttools.Tool{}})
	if !r.ReadOnly || r.Postgres.Status != "error" || r.ExitCode() == 0 {
		t.Fatalf("unexpected report: %#v", r)
	}
	raw, _ := json.Marshal(r)
	for _, secret := range []string{"synthetic-private-password", "postgres://", "password authentication failed"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("report exposed %q", secret)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("doctor created deployment files: %v, %v", entries, err)
	}
	if r.ReleaseCheck.Status != "unknown" {
		t.Fatal("doctor claimed a release had been verified without input")
	}
}

func TestDoctorOfflineSignatureAndArchiveVerification(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	previousRepo, previousKey := selfupdate.Repo, selfupdate.ReleasePublicKey
	selfupdate.Repo, selfupdate.ReleasePublicKey = "fixture/gondar", base64.StdEncoding.EncodeToString(public)
	t.Cleanup(func() { selfupdate.Repo, selfupdate.ReleasePublicKey = previousRepo, previousKey })
	dir := t.TempDir()
	archive := filepath.Join(dir, "synthetic-fixture.zip")
	manifest := filepath.Join(dir, "SHA256SUMS")
	signature := filepath.Join(dir, "SHA256SUMS.sig")
	payload := []byte("test fixture; never extract or execute")
	sum := sha256.Sum256(payload)
	sums := []byte(fmt.Sprintf("%x  %s\n", sum, filepath.Base(archive)))
	if err := os.WriteFile(archive, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(manifest, sums, 0o600)
	_ = os.WriteFile(signature, []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(private, sums))), 0o600)
	opts := Options{ReleaseManifest: manifest, ReleaseSignature: signature, ReleaseArchive: archive}
	if check := verifyReleaseFiles(opts); check.Status != "ok" {
		t.Fatalf("valid fixture: %#v", check)
	}
	_ = os.WriteFile(archive, []byte("modified"), 0o600)
	if check := verifyReleaseFiles(opts); check.Status != "error" {
		t.Fatal("accepted a modified archive")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Fatal("verification extracted or created extra files")
	}
	selfupdate.ReleasePublicKey = ""
	if check := verifyReleaseFiles(opts); check.Status != "error" {
		t.Fatal("accepted an archive without an already trusted key")
	}
}

func TestDoctorRejectsInvalidConfigWithoutPrintingContents(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(filename, []byte(`{"password":"synthetic-secret", broken`), 0o600)
	check := inspectConfig(filename)
	if check.Status != "error" || strings.Contains(check.Detail, "synthetic-secret") {
		t.Fatalf("invalid configuration diagnostic: %#v", check)
	}
}
