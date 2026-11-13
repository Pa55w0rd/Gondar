package traffic

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func legacyNullTraffic(t *testing.T) *Traffic {
	t.Helper()
	tr, err := Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	// These are real legacy SQLite rows, not Go zero-value fixtures. Leave all
	// omitted metadata NULL under the historical schema.
	for _, item := range []struct {
		id   string
		host any
	}{
		{"legacy-null", nil},
		{"legacy-empty", ""},
		{"legacy-with-body", "legacy.example"},
	} {
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,host) VALUES(?,?)`, item.id, item.host); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ id, host, url string }{
		{"normal", "normal.example", "https://normal.example/"},
		{"port-specific", "legacy.example", "https://legacy.example:8443/api"},
	} {
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,9,?,'GET','/api',?,201,'text/plain',7,8,'')`, item.id, item.host, item.url); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"legacy-with-body", "normal", "port-specific"} {
		if _, err := tr.DB().Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,resp_head,resp_body)
VALUES(?,'POST / HTTP/1.1',?,'HTTP/1.1 201 Created',?)`, id, []byte("request evidence"), []byte("response evidence")); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

func TestTrafficLegacyNullMetadataSearchPageAndHosts(t *testing.T) {
	tr := legacyNullTraffic(t)
	for _, read := range []struct {
		name string
		fn   func() ([]ExchangeMeta, error)
	}{
		{"search", func() ([]ExchangeMeta, error) { return tr.Search("", 0, 100) }},
		{"tool query", func() ([]ExchangeMeta, error) { return tr.query("", "", "", 0, 10) }},
		{"page", func() ([]ExchangeMeta, error) {
			rows, total, err := tr.Page(PageQuery{RespMin: -1, RespMax: -1}, 0, 100)
			if err == nil && total != 5 {
				t.Errorf("page total=%d want=5", total)
			}
			return rows, err
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			rows, err := read.fn()
			if err != nil || len(rows) != 5 {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
			seen := map[string]bool{}
			for _, row := range rows {
				seen[row.ID] = true
				if row.ID == "legacy-null" && row != (ExchangeMeta{ID: "legacy-null"}) {
					t.Errorf("NULL metadata must stay unknown: %+v", row)
				}
				if row.ID == "legacy-with-body" && (row.Status != 0 || row.TS != 0 || row.RespLen != 0 || row.Method != "" || row.Path != "") {
					t.Errorf("NULL metadata was fabricated: %+v", row)
				}
				if row.ID == "normal" && (row.Status != 201 || row.Method != "GET" || row.RespLen != 8 || row.TS != 9) {
					t.Errorf("normal metadata changed: %+v", row)
				}
			}
			if !seen["legacy-null"] || !seen["legacy-with-body"] || !seen["normal"] {
				t.Errorf("historical identities missing: %v", seen)
			}
		})
	}
	hosts, err := tr.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, host := range hosts {
		counts[host.Host] = host.Count
	}
	if len(counts) != 3 || counts[""] != 2 || counts["legacy.example"] != 2 || counts["normal.example"] != 1 {
		t.Fatalf("legacy host aggregation=%+v", hosts)
	}
	filtered, err := tr.query("legacy.example:8443", "", "", 0, 10)
	if err != nil || len(filtered) != 1 || filtered[0].ID != "port-specific" {
		t.Fatalf("NULL compatibility broadened authority scope: %+v err=%v", filtered, err)
	}
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		rows, total, err := tr.Page(PageQuery{RespMin: -1, RespMax: -1}, page, 2)
		if err != nil || total != 5 {
			t.Fatalf("page=%d total=%d err=%v", page, total, err)
		}
		for _, row := range rows {
			if seen[row.ID] {
				t.Fatalf("page repeated exchange identity %q", row.ID)
			}
			seen[row.ID] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("pagination omitted historical rows: %v", seen)
	}
	trees, err := tr.hostTrees("1=1")
	if err != nil || len(trees) != 2 {
		t.Fatalf("host trees=%v err=%v", trees, err)
	}
	for _, tree := range trees {
		if filepath.Base(tree) == "root" {
			t.Fatal("unknown host mapped to a real legacy tree")
		}
	}
	var untouched bool
	if err := tr.DB().QueryRow(`SELECT ts IS NULL AND status IS NULL AND req_len IS NULL AND resp_len IS NULL AND path IS NULL FROM exchanges WHERE id='legacy-with-body'`).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("metadata reads rewrote legacy row: untouched=%t err=%v", untouched, err)
	}
}

func TestTrafficLegacyNullLengthsPreserveBodiesAndEvidenceIdentity(t *testing.T) {
	tr := legacyNullTraffic(t)
	req, resp, err := tr.Get("legacy-with-body")
	if err != nil || !strings.Contains(req, "request evidence") || !strings.Contains(resp, "response evidence") {
		t.Fatalf("legacy body unavailable: req=%q resp=%q err=%v", req, resp, err)
	}
	called := false
	err = tr.ReadEvidence(context.Background(), []string{"legacy-with-body"}, func(e EvidenceExchange) error {
		called = true
		if e.ID != "legacy-with-body" || e.TS != 0 || e.Status != 0 || e.ReqLen != 0 || e.RespLen != 0 {
			t.Fatalf("evidence identity/unknown metadata changed: %+v", e)
		}
		req, err := io.ReadAll(e.Request)
		if err != nil {
			return err
		}
		resp, err := io.ReadAll(e.Response)
		if err != nil {
			return err
		}
		if string(req) != "request evidence" || string(resp) != "response evidence" {
			t.Errorf("evidence body changed: req=%q resp=%q", req, resp)
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("legacy evidence read failed: called=%t err=%v", called, err)
	}
	if _, _, err := tr.Get("legacy-null"); err == nil || !strings.Contains(err.Error(), "无正文记录") {
		t.Fatalf("missing body must not become successful empty evidence: %v", err)
	}
	if err := tr.ReadEvidence(context.Background(), []string{"legacy-null"}, func(EvidenceExchange) error {
		t.Fatal("missing body was consumed as evidence")
		return nil
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing evidence error=%v", err)
	}
}

func TestTrafficLegacyNullMetadataArchiveRoundTrip(t *testing.T) {
	tr := legacyNullTraffic(t)
	archive := t.TempDir()
	count, err := tr.ExportHosts([]string{"legacy.example"}, archive)
	if err != nil || count != 2 {
		t.Fatalf("legacy export count=%d err=%v", count, err)
	}
	restored, err := Open(t.TempDir(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	count, err = restored.ImportArchive(archive)
	if err != nil || count != 2 {
		t.Fatalf("legacy import count=%d err=%v", count, err)
	}
	if count, err := restored.ImportArchive(archive); err != nil || count != 0 {
		t.Fatalf("repeated import changed existing evidence: count=%d err=%v", count, err)
	}
	rows, err := restored.Search("", 0, 100)
	if err != nil || len(rows) != 2 {
		t.Fatalf("restored scope=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.Host != "legacy.example" {
			t.Fatalf("archive included a different host: %+v", row)
		}
		if row.ID == "legacy-with-body" && row.Status != 0 {
			t.Fatalf("archive fabricated legacy response status: %+v", row)
		}
		req, resp, err := restored.Get(row.ID)
		if err != nil || !strings.Contains(req, "request evidence") || !strings.Contains(resp, "response evidence") {
			t.Fatalf("archive lost evidence for original id=%q: req=%q resp=%q err=%v", row.ID, req, resp, err)
		}
	}
}
