package traffic

import (
	"log"
	"time"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

const (
	maxPendingCaptureBytes int64 = 128 << 20
	maxPendingCaptures           = 64
)

type CaptureStats struct {
	Recorded           int64 `json:"recorded"`
	Failed             int64 `json:"failed"`
	DroppedPressure    int64 `json:"dropped_pressure"`
	StreamedUnrecorded int64 `json:"streamed_unrecorded"`
	Pending            int   `json:"pending"`
	PendingBytes       int64 `json:"pending_bytes"`
	LastLossAt         int64 `json:"last_loss_at,omitempty"`
	MaxPendingBytes    int64 `json:"max_pending_bytes"`
	MaxPending         int   `json:"max_pending"`
}

// reserveCapture bounds buffered flows waiting on SQLite's single writer. It
// does not bound all network buffers. Saturation preserves forwarding and makes
// evidence loss observable instead of piling up unlimited waiters.
func (t *Traffic) reserveCapture(size int64) (func(), bool) {
	t.captureMu.Lock()
	if t.captureClosing || size < 0 || size > maxPendingCaptureBytes || t.capturePending >= maxPendingCaptures || t.captureBytes > maxPendingCaptureBytes-size {
		t.captureMu.Unlock()
		n := t.captureDropped.Add(1)
		t.captureLastLoss.Store(time.Now().Unix())
		if n == 1 || n&(n-1) == 0 {
			log.Printf("[traffic] 录制背压：累计跳过 %d 条交换，目标转发保持运行；证据覆盖不完整", n)
		}
		return nil, false
	}
	t.capturePending++
	t.captureBytes += size
	t.captureWG.Add(1)
	t.captureMu.Unlock()
	return func() {
		t.captureMu.Lock()
		t.capturePending--
		t.captureBytes -= size
		t.captureMu.Unlock()
		t.captureWG.Done()
	}, true
}

func (t *Traffic) CaptureStats() CaptureStats {
	t.captureMu.Lock()
	pending, size := t.capturePending, t.captureBytes
	t.captureMu.Unlock()
	return CaptureStats{Recorded: t.captureRecorded.Load(), Failed: t.captureFailed.Load(), DroppedPressure: t.captureDropped.Load(), StreamedUnrecorded: t.captureStreamed.Load(), Pending: pending, PendingBytes: size, LastLossAt: t.captureLastLoss.Load(), MaxPendingBytes: maxPendingCaptureBytes, MaxPending: maxPendingCaptures}
}

func captureSize(f *mproxy.Flow) int64 {
	n := int64(len(f.Request.Body)) + int64(len(f.Response.Body))
	for key, values := range f.Request.Header {
		n += int64(len(key))
		for _, value := range values {
			n += int64(len(value))
		}
	}
	for key, values := range f.Response.Header {
		n += int64(len(key))
		for _, value := range values {
			n += int64(len(value))
		}
	}
	return n
}
