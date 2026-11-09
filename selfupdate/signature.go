package selfupdate

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ReleasePublicKey is an Ed25519 public key, base64 encoded and fixed at build
// time. It is intentionally not a database/environment setting at runtime.
// An unset key does not mean SHA256SUMS has been authenticated.
var ReleasePublicKey string

const signatureAsset = "SHA256SUMS.sig"

var ErrReleaseKeyUnset = errors.New("此 Gondar 构建未配置可信发行公钥，拒绝安装未经签名验证的更新")

var releaseRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type VerificationPolicy struct {
	Repository       string `json:"repository,omitempty"`
	SourceConfigured bool   `json:"source_configured"`
	KeyConfigured    bool   `json:"key_configured"`
	Ready            bool   `json:"ready"`
	KeyFingerprint   string `json:"key_fingerprint,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

func ReleaseVerificationPolicy() VerificationPolicy {
	p := VerificationPolicy{SourceConfigured: releaseRepoPattern.MatchString(Repo)}
	if p.SourceConfigured {
		p.Repository = Repo
	}
	key, err := releaseVerificationKey()
	if err == nil {
		p.KeyConfigured = true
		sum := sha256.Sum256(key)
		p.KeyFingerprint = hex.EncodeToString(sum[:])
	}
	switch {
	case !p.SourceConfigured:
		p.Reason = ErrReleaseSourceUnset.Error()
	case err != nil:
		p.Reason = err.Error()
	default:
		p.Ready = true
	}
	return p
}

func releaseVerificationKey() (ed25519.PublicKey, error) {
	if strings.TrimSpace(ReleasePublicKey) == "" {
		return nil, ErrReleaseKeyUnset
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ReleasePublicKey))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("可信发行公钥格式错误：需要 base64 编码的 32 字节 Ed25519 公钥")
	}
	return ed25519.PublicKey(key), nil
}

// VerifyReleaseManifest verifies the exact SHA256SUMS bytes against a detached
// base64 Ed25519 signature. The key must already be trusted by this build;
// accepting a key advertised by the same untrusted download would prove nothing.
func VerifyReleaseManifest(manifest, signature []byte) error {
	if !releaseRepoPattern.MatchString(Repo) {
		return ErrReleaseSourceUnset
	}
	key, err := releaseVerificationKey()
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signature)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("发行签名格式错误")
	}
	if !ed25519.Verify(key, manifest, sig) {
		return fmt.Errorf("发行签名验证失败：清单被修改或签名不属于已信任的公钥")
	}
	return nil
}
