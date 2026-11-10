package hostd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Retry transient failures at the fixed URL. GitHub's download query offers a
// final retry for a stale/intermittently failing release redirect, preserving
// the exact repository, tag and asset. The caller still verifies the pinned hash.
// Digest failures never enter this retry path.
func fetchToolArchive(ctx context.Context, client *http.Client, address string) ([]byte, error) {
	addresses := []string{address, address}
	if alternate := githubDownloadRetryURL(address); alternate != "" {
		addresses = append(addresses, alternate)
	}
	for attempt, target := range addresses {
		blob, retry, err := fetchToolArchiveOnce(ctx, client, target)
		if err == nil {
			return blob, nil
		}
		if !retry || attempt == len(addresses)-1 || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("工具归档下载失败")
}

func githubDownloadRetryURL(address string) string {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 6 || parts[0] == "" || parts[1] == "" || parts[2] != "releases" || parts[3] != "download" || !exactVersion.MatchString(parts[4]) || parts[5] == "" {
		return ""
	}
	u.RawQuery = "download=1"
	return u.String()
}

func fetchToolArchiveOnce(ctx context.Context, client *http.Client, address string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, false, fmt.Errorf("固定归档地址无效")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("工具归档下载中断")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode >= 500, fmt.Errorf("归档下载失败：HTTP %d", resp.StatusCode)
	}
	blob, err := io.ReadAll(io.LimitReader(resp.Body, (128<<20)+1))
	if err != nil {
		return nil, true, fmt.Errorf("工具归档读取中断")
	}
	if len(blob) > 128<<20 {
		return nil, false, fmt.Errorf("工具归档超过大小限制")
	}
	return blob, false, nil
}
