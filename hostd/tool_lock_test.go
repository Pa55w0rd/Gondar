package hostd

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"gondar/hosttools"
)

func TestUnconfiguredRemoteToolCannotStartInstallJob(t *testing.T) {
	t.Setenv("PATH", "")
	s := NewServer(context.Background(), log.New(io.Discard, "", 0))
	s.ToolLockPath = filepath.Join(t.TempDir(), "not-configured.json")
	for _, id := range []string{"nuclei", "semgrep"} {
		if job, err := s.Start(context.Background(), id, ""); err == nil || job != nil {
			t.Fatalf("unconfigured tool %s started: job=%#v error=%v", id, job, err)
		}
	}
	if len(s.List()) != 0 {
		t.Fatal("created install jobs before validating a trusted lock")
	}
}

func TestToolArchiveDigestVerifiedBeforeExtraction(t *testing.T) {
	payload := []byte("synthetic archive; never execute")
	sum := sha256.Sum256(payload)
	pin := hosttools.InstallPin{SHA256: fmt.Sprintf("%x", sum)}
	if err := verifyToolArchive(payload, pin); err != nil {
		t.Fatal(err)
	}
	if err := verifyToolArchive([]byte("modified"), pin); err == nil {
		t.Fatal("accepted modified tool archive")
	}
	pin.SHA256 = strings.Repeat("0", 64)
	if err := verifyToolArchive(payload, pin); err == nil {
		t.Fatal("accepted wrong pinned archive digest")
	}
}
