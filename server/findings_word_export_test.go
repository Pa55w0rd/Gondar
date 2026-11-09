package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gondar/db"
	"gondar/report"
)

func TestWordBundleRendersEachFindingAndPropagatesFailure(t *testing.T) {
	longTitle := strings.Repeat("中文/../报告", 80)
	fs := []*db.DBFinding{{ID: 7, Name: longTitle, Evidence: "controlled proof"}, {ID: 8, Name: longTitle}}
	dest := filepath.Join(t.TempDir(), "reports.zip")
	err := writeWordBundle(t.Context(), dest, fs, func(f *db.DBFinding, w io.Writer) error {
		return report.BuildFindingDocx(report.FindingDocxInput{Finding: f, GeneratedAt: time.Unix(0, 0)}).Encode(w)
	})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := zip.OpenReader(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer outer.Close()
	if len(outer.File) != 2 {
		t.Fatalf("entries=%d", len(outer.File))
	}
	names := map[string]bool{}
	for _, entry := range outer.File {
		if names[entry.Name] || !utf8.ValidString(entry.Name) {
			t.Fatal("duplicate or invalid UTF8 Word member name")
		}
		names[entry.Name] = true
		if filepath.Base(entry.Name) != entry.Name {
			t.Fatal("unsafe archive member")
		}
		in, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(in)
		in.Close()
		if err != nil {
			t.Fatal(err)
		}
		inner, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := inner.Open("word/document.xml"); err != nil {
			t.Fatal(err)
		}
	}
	broken := filepath.Join(t.TempDir(), "failed.zip")
	sentinel := errors.New("controlled encode failure")
	err = writeWordBundle(t.Context(), broken, fs, func(f *db.DBFinding, w io.Writer) error {
		if f.ID == 8 {
			return sentinel
		}
		_, e := w.Write([]byte("prefix"))
		return e
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("lost encode error: %v", err)
	}
	if _, err := os.Stat(broken); !os.IsNotExist(err) {
		t.Fatalf("failed partial bundle remains: %v", err)
	}
}

func TestWordBundleCancellationAndSizeLimits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		canceled bool
	}{{"cancel", true}, {"oversize", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			dest := filepath.Join(t.TempDir(), "rejected.zip")
			err := writeWordBundle(ctx, dest, []*db.DBFinding{{ID: 1}}, func(_ *db.DBFinding, w io.Writer) error { _, e := w.Write(make([]byte, 33<<20)); return e })
			if err == nil {
				t.Fatal("expected cancellation or member budget error")
			}
			if _, e := os.Stat(dest); !os.IsNotExist(e) {
				t.Fatalf("partial remains: %v", e)
			}
		})
	}
}

func TestWordBundleHTTPRangeAndEvidenceMetadata(t *testing.T) {
	s, finding, request := trafficEvidenceServer(t)
	base := "/api/exploration/findings/export?format=docx-zip&scope=selected&ids="
	id := fmt.Sprint(finding.FindingID)
	for _, tc := range []struct {
		ids    string
		status int
	}{{"", 400}, {id + ",9223372036854775807", 409}, {strings.TrimSuffix(strings.Repeat(id+",", 101), ","), 413}} {
		w := request("GET", base+tc.ids, "")
		if w.Code != tc.status || w.Header().Get("Content-Disposition") != "" {
			t.Fatalf("invalid range status=%d want=%d body=%s", w.Code, tc.status, w.Body)
		}
	}
	seedServerEvidenceFlow(t, s, "word-proof", []byte("controlled immutable evidence"))
	bound, err := s.evidenceStore().Bind(t.Context(), finding.FindingID, []db.TrafficRef{{TrafficID: "word-proof", Role: "proof"}})
	if err != nil {
		t.Fatal(err)
	}
	w := request("GET", base+id, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("valid export: %d %s", w.Code, w.Body)
	}
	outer, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil || len(outer.File) != 1 {
		t.Fatalf("selected export incomplete: %v", err)
	}
	entry, err := outer.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(entry)
	entry.Close()
	if err != nil {
		t.Fatal(err)
	}
	doc, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	xml, err := doc.Open("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	defer xml.Close()
	data, err = io.ReadAll(xml)
	if err != nil {
		t.Fatal(err)
	}
	if len(bound.Bindings) != 1 || !bytes.Contains(data, []byte("关联流量证据")) || !bytes.Contains(data, []byte("请求头:")) {
		t.Fatal("Word report omitted the staged traffic metadata")
	}
}
