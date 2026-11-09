package report

import (
	"fmt"
	"gondar/db"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestFindingFilenameLongTitlesKeepDistinctIDs(t *testing.T) {
	for _, title := range []string{strings.Repeat("same-ascii-title", 30), strings.Repeat("中文长标题", 80)} {
		names := map[string]bool{}
		for _, id := range []int64{101, 102} {
			name := FindingFilename(&db.DBFinding{ID: id, Name: title, Severity: "high"})
			if !utf8.ValidString(name) || len(name) > 123 || !strings.HasSuffix(name, fmt.Sprintf("__%d.md", id)) || names[name] {
				t.Fatalf("long title lost stable unique identity: %q", name)
			}
			names[name] = true
		}
	}
}

func TestFindingMarkdownKeepsEvidenceInsideFenceAndWorker(t *testing.T) {
	f := &db.DBFinding{ID: 14, Name: strings.Repeat("中文", 50), Worker: "work#7", Evidence: "raw\n```\n# untrusted heading\n````\nend"}
	for _, body := range []string{FindingsMarkdown([]*db.DBFinding{f}, time.Unix(0, 0)), SingleFindingMarkdown(f, time.Unix(0, 0))} {
		if !strings.Contains(body, "**发现者**：work#7") || !strings.Contains(body, "`````\n"+f.Evidence+"\n`````") {
			t.Fatalf("evidence or author changed: %q", body)
		}
	}
	if name := FindingFilename(f); !utf8.ValidString(name) || len(name) > 123 || strings.ContainsAny(name, "/\\") {
		t.Fatalf("bad UTF8 filename: %q", name)
	}
}
