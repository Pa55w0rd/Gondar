package traffic

import (
	"io"
	"strings"
	"sync"
	"testing"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

func TestCaptureBackpressureBoundsAndRecovers(t *testing.T) {
	tr := &Traffic{}
	var releases []func()
	for i := 0; i < maxPendingCaptures; i++ {
		release, ok := tr.reserveCapture(1)
		if !ok {
			t.Fatal("capacity rejected early")
		}
		releases = append(releases, release)
	}
	if _, ok := tr.reserveCapture(1); ok {
		t.Fatal("unbounded waiting flows")
	}
	for _, release := range releases {
		release()
	}
	release, ok := tr.reserveCapture(maxPendingCaptureBytes)
	if !ok {
		t.Fatal("reservation did not recover")
	}
	if _, ok := tr.reserveCapture(1); ok {
		t.Fatal("unbounded pending bytes")
	}
	release()
	stats := tr.CaptureStats()
	if stats.Pending != 0 || stats.PendingBytes != 0 || stats.DroppedPressure != 2 || stats.LastLossAt == 0 {
		t.Fatalf("incorrect loss telemetry: %+v", stats)
	}
}

func TestSinkTelemetryPreservesStreamBytesAndReleasesOnFailure(t *testing.T) {
	tr, _ := openTraffic(t)
	s := &sink{t: tr}
	s.Response(newFlow("one.invalid", "GET", "/", nil, []byte("ok")))
	if tr.CaptureStats().Recorded != 1 {
		t.Fatal("successful recording not counted")
	}
	original := strings.NewReader("unchanged streamed evidence")
	r := s.StreamResponseModifier(&mproxy.Flow{Stream: true}, original)
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "unchanged streamed evidence" {
		t.Fatal("stream changed")
	}
	if _, err := tr.db.Exec(`DROP TABLE exchange_bodies`); err != nil {
		t.Fatal(err)
	}
	s.Response(newFlow("one.invalid", "GET", "/broken", nil, []byte("x")))
	stats := tr.CaptureStats()
	if stats.Failed != 1 || stats.StreamedUnrecorded != 1 || stats.Pending != 0 || stats.PendingBytes != 0 {
		t.Fatalf("failure leaked permit: %+v", stats)
	}
}

func TestCaptureConcurrentPermitAccounting(t *testing.T) {
	tr := &Traffic{}
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, ok := tr.reserveCapture(1024); ok {
				release()
			}
		}()
	}
	wg.Wait()
	stats := tr.CaptureStats()
	if stats.Pending != 0 || stats.PendingBytes != 0 {
		t.Fatalf("unbalanced reservations: %+v", stats)
	}
	tr.captureMu.Lock()
	tr.captureClosing = true
	tr.captureMu.Unlock()
	if _, ok := tr.reserveCapture(1); ok {
		t.Fatal("new recording admitted after shutdown")
	}
}

func TestProxyPoolReloadConcurrentSelection(t *testing.T) {
	s := &selector{}
	entries := []PoolEntry{{URL: "http://127.0.0.1:8081", Enabled: true, Weight: 1}, {URL: "http://127.0.0.1:8082", Enabled: true, Weight: 2}}
	if err := s.SetPool(entries, "round_robin"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := s.SetPool(entries, "round_robin"); err != nil {
					t.Error(err)
				}
				if u := s.Pick("one.invalid"); u != nil {
					s.ReportResult(u.String(), true)
				}
			}
		}()
	}
	wg.Wait()
	if s.Pick("one.invalid") == nil {
		t.Fatal("pool lost after reload")
	}
}
