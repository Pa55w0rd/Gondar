package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type fallbackTransport func(*http.Request) (*http.Response, error)

func (f fallbackTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRateLimitedReleaseStillAuthenticatesSameRepoAndVersion(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oldRepo, oldKey := Repo, ReleasePublicKey
	Repo, ReleasePublicKey = "fixture/gondar", base64.StdEncoding.EncodeToString(public)
	t.Cleanup(func() { Repo, ReleasePublicKey = oldRepo, oldKey })
	name := AssetName("v1.2.3", "linux", "amd64")
	manifest := []byte(strings.Repeat("a", 64) + "  " + name + "\n")
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(private, manifest))
	for _, mode := range []string{"latest", "tag", "wrong repo", "wrong version", "tampered signature"} {
		t.Run(mode, func(t *testing.T) {
			client := &http.Client{Transport: fallbackTransport(func(r *http.Request) (*http.Response, error) {
				response := &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader(""))}
				if r.URL.Host == "api.github.com" {
					response.StatusCode = 403
					return response, nil
				}
				if r.Method == http.MethodHead {
					target := "https://github.com/fixture/gondar/releases/tag/v1.2.3"
					if mode == "wrong repo" {
						target = "https://github.com/other/repo/releases/tag/v1.2.3"
					}
					if mode == "wrong version" {
						target = "https://github.com/fixture/gondar/releases/tag/v9.9.9"
					}
					copy := r.Clone(r.Context())
					copy.URL, _ = url.Parse(target)
					response.Request = copy
					return response, nil
				}
				if !strings.HasPrefix(r.URL.String(), "https://github.com/fixture/gondar/releases/download/v1.2.3/") {
					t.Fatalf("unexpected asset source: %s", r.URL)
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
					response.Body = io.NopCloser(strings.NewReader(string(manifest)))
				case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.sig"):
					data := signature
					if mode == "tampered signature" {
						data = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
					}
					response.Body = io.NopCloser(strings.NewReader(data))
				default:
					t.Fatal("discovery downloaded an executable archive")
				}
				return response, nil
			})}
			var rel *Release
			var err error
			if mode == "tag" || mode == "wrong version" {
				rel, err = FetchByTag(t.Context(), client, "1.2.3")
			} else {
				rel, err = FetchLatest(t.Context(), client)
			}
			if strings.HasPrefix(mode, "wrong") || mode == "tampered signature" {
				if err == nil || rel != nil {
					t.Fatalf("unsafe fallback accepted: %+v %v", rel, err)
				}
				return
			}
			if err != nil || rel.TagName != "v1.2.3" {
				t.Fatalf("fallback failed: %v", err)
			}
			if got, ok := rel.FindAsset(name); !ok || !strings.Contains(got.URL, "/v1.2.3/"+name) {
				t.Fatalf("missing pinned archive: %+v", rel)
			}
		})
	}
}
