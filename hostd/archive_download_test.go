package hostd

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestGitHubTransientDownloadRetryPreservesExactAsset(t *testing.T) {
	address := "https://github.com/peass-ng/PEASS-ng/releases/download/20261107-fixture/linpeas.sh"
	attempts := 0
	client := &http.Client{Transport: versionTransport(func(r *http.Request) (*http.Response, error) {
		attempts++
		want := address
		if attempts == 3 {
			want += "?download=1"
		}
		if r.URL.String() != want {
			t.Fatalf("retry changed source, version or asset: %s", r.URL)
		}
		if attempts < 3 {
			resp := fakeResponse("upstream timeout")
			resp.StatusCode = http.StatusGatewayTimeout
			return resp, nil
		}
		return fakeResponse("verified by the caller"), nil
	})}
	blob, err := fetchToolArchive(context.Background(), client, address)
	if err != nil || string(blob) != "verified by the caller" || attempts != 3 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	for _, bad := range []string{
		strings.Replace(address, "github.com", "example.com", 1),
		strings.Replace(address, "https://", "http://", 1),
		strings.Replace(address, "releases/download", "raw", 1),
		address + "?another=1", address + "#fragment",
	} {
		if got := githubDownloadRetryURL(bad); got != "" {
			t.Fatalf("alternate URL accepted for noncanonical asset: %s", bad)
		}
	}
}
