package filepreview

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func zipDirectoryFixture(t *testing.T, count int) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for i := 0; i < count; i++ {
		w, err := z.Create("item.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("synthetic")); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), b.Bytes()...)
}

func TestZIPListingLongNamesHasWireBudget(t *testing.T) {
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	for i := 0; i < 8; i++ {
		if _, err := z.Create(strings.Repeat("n", 10000) + string(rune('a'+i)) + ".txt"); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	p, err := Read(fixture(t, "long-names.zip", out.Bytes()), "long-names.zip", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p)
	if err != nil || !p.Truncated || len(raw) > MaxBytes {
		t.Fatalf("directory output unbounded: bytes=%d truncated=%v err=%v", len(raw), p.Truncated, err)
	}
}

func checkDirectoryFixture(t *testing.T, b []byte) error {
	t.Helper()
	p := fixture(t, "directory.zip", b)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return checkZipDirectory(f, int64(len(b)))
}

func TestZIPChecksRealDirectoryBeforeStandardParser(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]byte, int, int)
	}{
		{"small_count", func(b []byte, e, c int) {
			binary.LittleEndian.PutUint16(b[e+8:e+10], 1)
			binary.LittleEndian.PutUint16(b[e+10:e+12], 1)
		}},
		{"small_size", func(b []byte, e, c int) { binary.LittleEndian.PutUint32(b[e+12:e+16], 1) }},
		// Pretend only the final directory record exists. Go's base fallback
		// would read all records at the unchanged original offset.
		{"hidden_prefix", func(b []byte, e, c int) {
			binary.LittleEndian.PutUint16(b[e+8:e+10], 1)
			binary.LittleEndian.PutUint16(b[e+10:e+12], 1)
			binary.LittleEndian.PutUint32(b[e+12:e+16], uint32(e-(c+(46+len("item.txt"))*(maxZipEntries))))
		}},
		{"multidisk_count", func(b []byte, e, c int) { binary.LittleEndian.PutUint16(b[e+8:e+10], 0) }},
		{"member_disk", func(b []byte, e, c int) { binary.LittleEndian.PutUint16(b[c+34:c+36], 1) }},
		{"zip64_offset", func(b []byte, e, c int) { binary.LittleEndian.PutUint32(b[e+16:e+20], 0xffffffff) }},
		{"zip64_member", func(b []byte, e, c int) { binary.LittleEndian.PutUint32(b[c+24:c+28], 0xffffffff) }},
		{"header_length", func(b []byte, e, c int) { binary.LittleEndian.PutUint16(b[c+28:c+30], 65535) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 3
			if tc.name == "hidden_prefix" {
				n = maxZipEntries + 1
			}
			b := zipDirectoryFixture(t, n)
			e := len(b) - 22
			c := int(binary.LittleEndian.Uint32(b[e+16 : e+20]))
			tc.mutate(b, e, c)
			if err := checkDirectoryFixture(t, b); err == nil {
				t.Fatal("forged directory passed allocation preflight")
			}
		})
	}
}

func TestZIPAmbiguousEOCDAndUnsupportedExtraAreRejected(t *testing.T) {
	// A second EOCD-looking header inside the comment would be preferred by
	// archive/zip, while a strict EOF-only selector would check the earlier one.
	b := zipDirectoryFixture(t, 1)
	end := len(b) - 22
	comment := make([]byte, 30)
	binary.LittleEndian.PutUint32(comment[:4], 0x06054b50)
	binary.LittleEndian.PutUint16(b[end+20:end+22], uint16(len(comment)))
	b = append(b, comment...)
	if err := checkDirectoryFixture(t, b); err == nil {
		t.Fatal("ambiguous EOCD selector accepted")
	}
	var out bytes.Buffer
	z := zip.NewWriter(&out)
	if _, err := z.CreateHeader(&zip.FileHeader{Name: "zip64.txt", Extra: []byte{1, 0, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkDirectoryFixture(t, out.Bytes()); err == nil {
		t.Fatal("ZIP64 extra field accepted")
	}
}

func TestZIPOrdinaryPrefixAndCompressedMemberRemainBounded(t *testing.T) {
	b := zipDirectoryFixture(t, 1)
	if err := checkDirectoryFixture(t, b); err != nil {
		t.Fatal(err)
	}
	// A harmless preamble is accepted when Go's base offset is unambiguous.
	prefixed := append(bytes.Repeat([]byte{'p'}, 64), b...)
	p, err := Read(fixture(t, "prefixed.zip", prefixed), "prefixed.zip", "item.txt")
	if err != nil || p.Text != "synthetic" {
		t.Fatalf("ordinary prefix: %+v %v", p, err)
	}
	var large bytes.Buffer
	z := zip.NewWriter(&large)
	w, err := z.Create("large.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(strings.Repeat("中文", MaxBytes))); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = Read(fixture(t, "bounded.zip", large.Bytes()), "bounded.zip", "large.txt")
	if err != nil || !p.Truncated || len(p.Text) > MaxBytes || strings.Contains(p.Text, "�") {
		t.Fatalf("compressed prefix exceeded bounds: %+v %v", p, err)
	}
}
