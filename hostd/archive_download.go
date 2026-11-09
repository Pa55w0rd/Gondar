package hostd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Retry a transient transport/5xx failure once against the SAME fixed URL.
// Digest failures are checked by the caller and never enter this retry path.
func fetchToolArchive(ctx context.Context, client *http.Client, address string) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		blob, retry, err := fetchToolArchiveOnce(ctx, client, address)
		if err == nil {
			return blob, nil
		}
		if !retry || attempt == 1 || ctx.Err() != nil {
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
