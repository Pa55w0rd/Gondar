package hosttools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestImageMagickUsesActualCommandsAndStableID(t *testing.T) {
	tool, ok := Lookup("imagemagick")
	if !ok || tool.ID != "imagemagick" {
		t.Fatal("ImageMagick API identity changed")
	}
	for _, bin := range []string{"magick", "convert"} {
		t.Run(bin, func(t *testing.T) {
			dir := t.TempDir()
			name := bin
			if runtime.GOOS == "windows" {
				name += ".exe"
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("not executed"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			got, ok := tool.availableBin()
			if !ok || got != bin {
				t.Fatalf("installed command was not detected: %s %v", got, ok)
			}
		})
	}
}

func TestInstalledVersionRequiresMatchingFileAndTool(t *testing.T) {
	dir := t.TempDir()
	name := "linpeas"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	filename := filepath.Join(dir, name)
	data := []byte("synthetic; never execute")
	if err := os.WriteFile(filename, data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	type testCase struct{ id, body, expected string }
	digest := sha256.Sum256(data)
	for _, tc := range []testCase{{"linpeas", string(data), "20261107-6821b3db"}, {"other", string(data), ""}, {"linpeas", "modified", ""}} {
		raw, _ := json.Marshal(InstalledRecord{ToolID: tc.id, Version: "20261107-6821b3db", SHA256: hex.EncodeToString(digest[:])})
		if err := os.WriteFile(filename+".gondar-version.json", raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(tc.body), 0o755); err != nil {
			t.Fatal(err)
		}
		tool, _ := Lookup("linpeas")
		got := ProbeInstalled(t.Context(), tool)
		if !got.Available || got.Version != tc.expected {
			t.Fatalf("unbound metadata accepted: %+v", got)
		}
	}
}

func TestEnumerationVersionProbeDoesNotExecute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, id := range []string{"linpeas", "pspy64", "responder"} {
		t.Run(id, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "executed")
			tool, _ := Lookup(id)
			if err := os.WriteFile(filepath.Join(dir, tool.Bin), []byte("#!/bin/sh\necho bad > '"+marker+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			if got := ProbeInstalled(t.Context(), tool); !got.Available || got.Version != "" {
				t.Fatalf("unexpected state: %+v", got)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("version check ran an enumerator")
			}
		})
	}
}
