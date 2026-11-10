package hostd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"gondar/hosttools"
)

type metadataStatusError struct {
	Status      int
	RateLimited bool
}

func (e *metadataStatusError) Error() string {
	if e.RateLimited {
		return "GitHub公共接口请求额度已用完（HTTP 403/429），并非工具损坏；请稍后重试或检查安装代理"
	}
	return fmt.Sprintf("官方服务拒绝或无法完成请求（HTTP %d），请检查网络后重试", e.Status)
}

// API rate limits do not disable discovery. The fallback still requires the
// exact official repository/tag and a checksum for the exact platform asset.
func githubReleaseFallback(ctx context.Context, client *http.Client, tool hosttools.Tool, info *ToolVersion, original error) (*hosttools.InstallPin, error) {
	var status *metadataStatusError
	if !errors.As(original, &status) || (status.Status != 403 && status.Status != 429) {
		return nil, original
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, "https://github.com/"+tool.Repo+"/releases/latest", nil)
	req.Header.Set("User-Agent", "Gondar-managed-tools")
	resp, err := client.Do(req)
	if err != nil {
		return nil, original
	}
	resp.Body.Close()
	prefix := "/" + tool.Repo + "/releases/tag/"
	if resp.StatusCode != 200 || resp.Request == nil || resp.Request.URL.Scheme != "https" || resp.Request.URL.Host != "github.com" || !strings.HasPrefix(resp.Request.URL.Path, prefix) {
		return nil, original
	}
	tag := strings.TrimPrefix(resp.Request.URL.Path, prefix)
	if !exactVersion.MatchString(tag) || strings.Contains(tag, "..") || strings.EqualFold(tag, "latest") {
		return nil, original
	}
	info.LatestVersion = tag
	asset := path.Base(GitHubAssetURL(tool, tag))
	// New GitHub assets expose their publisher digest on the official expanded
	// asset page even when the REST API quota is exhausted.
	expanded := "https://github.com/" + tool.Repo + "/releases/expanded_assets/" + url.PathEscape(tag)
	if data, pageErr := metadata(ctx, client, expanded, 2<<20); pageErr == nil {
		if digest, digestErr := expandedAssetDigest(data, "/"+tool.Repo+"/releases/download/"+tag+"/"+asset); digestErr == nil {
			info.SHA256 = digest
			return &hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo, Version: tag, SHA256: digest}, nil
		}
	}
	version := strings.TrimPrefix(tag, "v")
	name := path.Base(tool.Repo)
	for _, file := range []string{name + "-checksums.txt", name + "_" + version + "_checksums.txt", name + "_checksums.txt", "checksums.txt", "SHA256SUMS"} {
		address := "https://github.com/" + tool.Repo + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(file)
		data, err := metadata(ctx, client, address, 1<<20)
		if err != nil {
			continue
		}
		if digest, ok := checksumFor(data, asset); ok {
			info.SHA256 = digest
			return &hosttools.InstallPin{ToolID: tool.ID, Kind: tool.Kind, Repository: tool.Repo, Version: tag, SHA256: digest}, nil
		}
	}
	return nil, fmt.Errorf("检测到%s，但官方同版本安装包校验信息暂不可用；请重试", tag)
}
