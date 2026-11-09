package selfupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func trustFixtureKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	previousRepo, previousKey := Repo, ReleasePublicKey
	Repo, ReleasePublicKey = "fixture/gondar", base64.StdEncoding.EncodeToString(public)
	t.Cleanup(func() { Repo, ReleasePublicKey = previousRepo, previousKey })
	return private
}

func TestReleaseManifestTrustAndTampering(t *testing.T) {
	private := trustFixtureKey(t)
	manifest := []byte(strings.Repeat("a", 64) + "  gondar-fixture.zip\n")
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(private, manifest)))
	if err := VerifyReleaseManifest(manifest, signature); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReleaseManifest(append(append([]byte{}, manifest...), '\n'), signature); err == nil {
		t.Fatal("accepted a changed manifest")
	}
	if err := VerifyReleaseManifest(manifest, []byte("not-a-signature")); err == nil {
		t.Fatal("accepted an invalid signature")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	ReleasePublicKey = base64.StdEncoding.EncodeToString(other)
	if err := VerifyReleaseManifest(manifest, signature); err == nil {
		t.Fatal("accepted a signature from another key")
	}
	ReleasePublicKey = ""
	if err := VerifyReleaseManifest(manifest, signature); err == nil {
		t.Fatal("accepted an unconfigured trust key")
	}
	if ReleaseVerificationPolicy().Ready {
		t.Fatal("reported an unconfigured signer as ready")
	}
}

func TestOpenSSLDetachedSignatureCompatibility(t *testing.T) {
	openssl, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("需要本地 OpenSSL，仅验证 Go 与发行 CI 的离线签名格式兼容性")
	}
	private := trustFixtureKey(t)
	dir := t.TempDir()
	keyFile, manifestFile, sigFile := filepath.Join(dir, "TEST_ONLY.pem"), filepath.Join(dir, "SHA256SUMS"), filepath.Join(dir, "signature.raw")
	encoded, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(strings.Repeat("a", 64) + "  synthetic-fixture.zip\n")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestFile, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(openssl, "pkeyutl", "-sign", "-rawin", "-inkey", keyFile, "-in", manifestFile, "-out", sigFile).CombinedOutput(); err != nil {
		t.Fatalf("OpenSSL sign: %v: %s", err, output)
	}
	signature, err := os.ReadFile(sigFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyReleaseManifest(manifest, []byte(base64.StdEncoding.EncodeToString(signature))); err != nil {
		t.Fatalf("Go rejected the CI signature format: %v", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(private.Public())
	if err != nil {
		t.Fatal(err)
	}
	publicFile := filepath.Join(dir, "public.der")
	if err := os.WriteFile(publicFile, publicDER, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sigFile, ed25519.Sign(private, manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(openssl, "pkeyutl", "-verify", "-rawin", "-pubin", "-keyform", "DER", "-inkey", publicFile, "-in", manifestFile, "-sigfile", sigFile).CombinedOutput(); err != nil {
		t.Fatalf("OpenSSL rejected Go signature: %v: %s", err, output)
	}
}

type releaseFixtureTransport struct {
	t        *testing.T
	files    map[string]string
	zipCalls int
	allCalls int
}

func (f *releaseFixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.allCalls++
	if strings.HasSuffix(req.URL.Path, ".zip") {
		f.zipCalls++
	}
	data, ok := f.files[req.URL.String()]
	if !ok {
		return nil, fmt.Errorf("unexpected fixture URL")
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data)), Request: req}, nil
}

func TestVerifiedZipRequiresTrustedSignatureBeforeDownload(t *testing.T) {
	for _, mode := range []string{"valid", "tampered_manifest", "unsigned", "no_trusted_key"} {
		t.Run(mode, func(t *testing.T) {
			private := trustFixtureKey(t)
			name := AssetName("v1.3.0", runtime.GOOS, runtime.GOARCH)
			payload := "synthetic release archive; never executed"
			digest := sha256.Sum256([]byte(payload))
			manifest := fmt.Sprintf("%x  %s\n", digest, name)
			signature := base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte(manifest)))
			if mode == "tampered_manifest" {
				manifest += "\n"
			}
			base := "https://github.com/fixture/gondar/releases/download/v1.3.0/"
			transport := &releaseFixtureTransport{t: t, files: map[string]string{base + sumsAsset: manifest, base + signatureAsset: signature, base + name: payload}}
			release := &Release{TagName: "v1.3.0", Assets: []Asset{{Name: sumsAsset, URL: base + sumsAsset}, {Name: name, URL: base + name}}}
			if mode != "unsigned" {
				release.Assets = append(release.Assets, Asset{Name: signatureAsset, URL: base + signatureAsset})
			}
			if mode == "no_trusted_key" {
				ReleasePublicKey = ""
			}
			filename, err := FetchVerifiedZip(context.Background(), &http.Client{Transport: transport}, release, t.TempDir(), nil)
			if mode == "valid" {
				if err != nil || transport.zipCalls != 1 {
					t.Fatalf("valid signed release: %v, zip calls=%d", err, transport.zipCalls)
				}
				got, err := os.ReadFile(filename)
				if err != nil || string(got) != payload {
					t.Fatalf("verified archive=%q, error=%v", got, err)
				}
			} else if err == nil || transport.zipCalls != 0 {
				t.Fatalf("untrusted release: error=%v, zip calls=%d", err, transport.zipCalls)
			}
			if mode == "no_trusted_key" && transport.allCalls != 0 {
				t.Fatal("contacted release service without configured trust key")
			}
		})
	}
}
