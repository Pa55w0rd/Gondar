package db

import "testing"

func TestAssetRoleHintsUseSegmentsNotSubstrings(t *testing.T) {
	cases := []struct {
		host, url string
		want      int
	}{
		{"console.example.invalid", "https://console.example.invalid/api/login", 3},
		{"notadmin.example.invalid", "https://notadmin.example.invalid/notadmin?next=/admin", 0},
		{"example.invalid", "https://example.invalid/administrator", 0},
		{"login.example.invalid", "https://login.example.invalid/login", 1},
		{"storage.example.invalid", "", 1},
	}
	for _, tc := range cases {
		got := AssetRoleHints(tc.host, tc.url)
		if len(got) != tc.want {
			t.Fatalf("%s %s: %+v, want %d", tc.host, tc.url, got, tc.want)
		}
	}
}
