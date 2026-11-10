package hostd

import (
	"strings"
	"testing"
)

func TestAssetPageDigestCannotCrossAssetRepositoryOrTag(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	want := "/peass-ng/PEASS-ng/releases/download/v1/linpeas_linux_amd64"
	row := func(path, d string) string {
		return `<li><a href="` + path + `">download</a><clipboard-copy value="sha256:` + d + `"></clipboard-copy></li>`
	}
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"exact", row(want, a), true},
		{"other asset", row(strings.ReplaceAll(want, "amd64", "arm64"), a), false},
		{"other tag", row(strings.ReplaceAll(want, "/v1/", "/v2/"), a), false},
		{"other repo", row(strings.ReplaceAll(want, "peass-ng/PEASS-ng", "evil/tool"), a), false},
		{"digest elsewhere", `<li><a href="` + want + `">asset</a></li><li><clipboard-copy value="sha256:` + a + `"></clipboard-copy></li>`, false},
		{"conflicting repeated asset", row(want, a) + row(want, b), false},
		{"ambiguous row", `<li><a href="` + want + `">one</a><a href="/other/repo/releases/download/v1/other">two</a><clipboard-copy value="sha256:` + a + `"></clipboard-copy></li>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandedAssetDigest([]byte(tc.body), want)
			if (err == nil) != tc.ok || tc.ok && got != a {
				t.Fatalf("digest=%s err=%v", got, err)
			}
		})
	}
}
