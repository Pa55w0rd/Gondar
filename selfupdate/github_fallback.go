package selfupdate

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The public API and release pages have separate availability limits. A page
// redirect selects the tag, but only the pinned signing key authenticates assets.
func signedReleaseFallback(ctx context.Context, c *http.Client, apiURL string) (*Release, error) {
	if !releaseRepoPattern.MatchString(Repo) {
		return nil, ErrReleaseSourceUnset
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	page := "https://github.com/" + Repo + "/releases/latest"
	wanted := ""
	if apiURL != latestURL() {
		wanted = strings.TrimPrefix(apiURL, "https://api.github.com/repos/"+Repo+"/releases/tags/")
		if !ValidVersionTag(wanted) {
			return nil, fmt.Errorf("发行版本号无效")
		}
		page = "https://github.com/" + Repo + "/releases/tag/" + url.PathEscape(wanted)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, page, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gondar-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	prefix := "/" + Repo + "/releases/tag/"
	if resp.StatusCode != http.StatusOK || resp.Request == nil || resp.Request.URL.Scheme != "https" || resp.Request.URL.Host != "github.com" || resp.Request.URL.User != nil || !strings.HasPrefix(resp.Request.URL.Path, prefix) {
		return nil, fmt.Errorf("官方发行页不可用或重定向离开指定仓库")
	}
	tag := strings.TrimPrefix(resp.Request.URL.Path, prefix)
	if !ValidVersionTag(tag) {
		return nil, fmt.Errorf("官方发行页未返回正式版本号")
	}
	if wanted != "" {
		if cmp, ok := CompareVersions(wanted, tag); !ok || cmp != 0 {
			return nil, fmt.Errorf("官方发行页返回了其他版本")
		}
	}
	asset := func(name string) Asset {
		return Asset{Name: name, URL: "https://github.com/" + Repo + "/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)}
	}
	rel := &Release{TagName: tag, Name: "Gondar " + tag, HTMLURL: resp.Request.URL.String(), Assets: []Asset{asset(sumsAsset), asset(signatureAsset)}}
	sums, err := fetchSums(ctx, c, rel)
	if err != nil {
		return nil, err
	}
	for _, target := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"darwin", "amd64"}, {"darwin", "arm64"}, {"windows", "amd64"}} {
		name := AssetName(tag, target[0], target[1])
		if _, ok := sums[name]; ok {
			rel.Assets = append(rel.Assets, asset(name))
		}
	}
	if len(rel.Assets) == 2 {
		return nil, fmt.Errorf("签名清单没有该版本的受支持安装包")
	}
	return rel, nil
}
