package filepreview

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBoundedTextAndCSV(t *testing.T) {
	name := fixture(t, "big.txt", []byte(strings.Repeat("中文数据", MaxBytes)))
	p, err := Read(name, "big.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || len(p.Text) > MaxBytes || strings.Contains(p.Text, "�") {
		t.Fatalf("invalid bounded UTF-8 view: %#v", p)
	}
	name = fixture(t, "data.csv", []byte("name,description\r\n甲,\"第一行\n第二行\"\r\n乙,3\r\n"))
	p, err = Read(name, "data.csv", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Truncated || len(p.Rows) != 3 || p.Rows[1][1] != "第一行\n第二行" {
		t.Fatalf("CSV fields lost: %#v", p)
	}
	name = fixture(t, "binary.txt", []byte{'a', 0, 'b'})
	if _, err = Read(name, "binary.txt", ""); err == nil {
		t.Fatal("binary accepted as text")
	}
}

func TestGzipBombIsPrefixOnly(t *testing.T) {
	var b bytes.Buffer
	g := gzip.NewWriter(&b)
	if _, err := g.Write(bytes.Repeat([]byte("x"), 4<<20)); err != nil {
		t.Fatal(err)
	}
	g.Close()
	name := fixture(t, "large.gz", b.Bytes())
	p, err := Read(name, "large.gz", "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || len(p.Text) != MaxBytes || p.Format != "gzip/text" {
		t.Fatalf("unbounded expansion: %#v", p)
	}
}

func TestZIPListingMemberAndTraversal(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, name := range []string{"table.csv", "../escape.txt", "nested.zip"} {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("a,b\n1,2\n"))
	}
	z.Close()
	name := fixture(t, "bundle.zip", b.Bytes())
	p, err := Read(name, "bundle.zip", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 3 || p.Truncated {
		t.Fatalf("listing: %#v", p)
	}
	p, err = Read(name, "bundle.zip", "table.csv")
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != "zip/csv" || len(p.Rows) != 2 {
		t.Fatalf("member: %#v", p)
	}
	for _, entry := range []string{"../escape.txt", "nested.zip", "missing.txt"} {
		if _, err := Read(name, "bundle.zip", entry); err == nil {
			t.Fatalf("unsafe/missing member accepted: %s", entry)
		}
	}
}

func TestZIPExcessDirectoryRejectedBeforeAllocation(t *testing.T) {
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for i := 0; i < maxZipEntries+1; i++ {
		if _, err := z.Create("x"); err != nil {
			t.Fatal(err)
		}
	}
	z.Close()
	name := fixture(t, "many.zip", b.Bytes())
	if _, err := Read(name, "many.zip", ""); err == nil {
		t.Fatal("oversized directory accepted")
	}
}

func TestRowsAndColumnsAreExplicitlyTruncated(t *testing.T) {
	row := strings.Repeat("x,", MaxColumns+1) + "end\n"
	p, err := Read(fixture(t, "wide.csv", []byte(strings.Repeat(row, MaxRows+2))), "wide.csv", "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || len(p.Rows) != MaxRows || len(p.Rows[0]) != MaxColumns {
		t.Fatalf("bounds lost: %#v", p)
	}
}
