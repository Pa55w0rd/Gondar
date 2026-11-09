package traffic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	uuid "github.com/satori/go.uuid"
)

func TestRepeaterCaptureExactCommitAndPrivateHeader(t *testing.T) {
	tr, _ := openTraffic(t)
	ticket := tr.NewRepeaterCapture("one.invalid")
	defer ticket.Close()
	f := newFlow("one.invalid", "GET", "/", nil, []byte("one"))
	f.Id = uuid.NewV4()
	f.Request.Header.Set(RepeaterCaptureHeader, ticket.Token())
	s := &sink{t: tr}
	s.Requestheaders(f)
	if f.Request.Header.Get(RepeaterCaptureHeader) != "" {
		t.Fatal("private marker survived")
	}
	// Unrelated concurrent flow must never become this ticket's evidence.
	other := newFlow("one.invalid", "GET", "/other", nil, []byte("other"))
	other.Id = uuid.NewV4()
	s.Response(other)
	s.Response(f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := ticket.Wait(ctx)
	if result.TrafficID == "" || result.TrafficID == f.Id.String() {
		t.Fatalf("not actual recorder ID: %+v", result)
	}
	req, resp, err := tr.Get(result.TrafficID)
	if err != nil || !strings.Contains(resp, "one") || strings.Contains(req, "/other") || strings.Contains(req, RepeaterCaptureHeader) {
		t.Fatalf("wrong evidence: %v %q %q", err, req, resp)
	}
	ticket.Close()
	ticket.Close()
	if tr.repeaterMustIntercept("one.invalid") {
		t.Fatal("MITM lease leaked")
	}
	if _, ok := repeaterCaptureTickets.Load(ticket.Token()); ok {
		t.Fatal("ticket leaked")
	}
	invalid := newFlow("one.invalid", "GET", "/", nil, nil)
	invalid.Request.Header.Set(RepeaterCaptureHeader, "unregistered")
	s.Requestheaders(invalid)
	if invalid.Request.Header.Get(RepeaterCaptureHeader) != "" {
		t.Fatal("unknown private marker survived")
	}
}

func TestRepeaterCaptureLossPathsCompleteWithoutGuessing(t *testing.T) {
	for _, mode := range []string{"pressure", "stream", "record-error", "request-error"} {
		t.Run(mode, func(t *testing.T) {
			tr, _ := openTraffic(t)
			s := &sink{t: tr}
			ticket := tr.NewRepeaterCapture("one.invalid")
			defer ticket.Close()
			f := newFlow("one.invalid", "GET", "/", nil, []byte("x"))
			f.Id = uuid.NewV4()
			f.Request.Header.Set(RepeaterCaptureHeader, ticket.Token())
			s.Requestheaders(f)
			switch mode {
			case "pressure":
				release, ok := tr.reserveCapture(maxPendingCaptureBytes)
				if !ok {
					t.Fatal("reserve")
				}
				defer release()
				s.Response(f)
			case "stream":
				f.Stream = true
				s.StreamResponseModifier(f, strings.NewReader("x"))
			case "record-error":
				if _, err := tr.db.Exec(`DROP TABLE exchange_bodies`); err != nil {
					t.Fatal(err)
				}
				s.Response(f)
			case "request-error":
				s.RequestError(f, errors.New("synthetic request failure"))
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result := ticket.Wait(ctx)
			if result.TrafficID != "" || !strings.Contains(result.Note, "未落盘") {
				t.Fatalf("loss did not complete explicitly: %+v", result)
			}
		})
	}
}

func TestRepeaterCaptureOwnerLeaseAndCancellation(t *testing.T) {
	a, _ := openTraffic(t)
	b, _ := openTraffic(t)
	ticket := a.NewRepeaterCapture("one.invalid")
	defer ticket.Close()
	a.pass.Store("one.invalid", struct{}{}) // Another request toggled passthrough after initial check.
	if !a.repeaterMustIntercept("one.invalid:443") || b.repeaterMustIntercept("one.invalid") {
		t.Fatal("lease not confined to recorder/host")
	}
	f := newFlow("one.invalid", "GET", "/", nil, nil)
	f.Id = uuid.NewV4()
	f.Request.Header.Set(RepeaterCaptureHeader, ticket.Token())
	(&sink{t: b}).Requestheaders(f)
	if f.Request.Header.Get(RepeaterCaptureHeader) != "" {
		t.Fatal("non-owner leaked private marker")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := ticket.Wait(ctx); result.TrafficID != "" || !strings.Contains(result.Note, "context canceled") {
		t.Fatal("canceled ticket blocked")
	}
	ticket.Close()
	// A late addon callback cannot panic, republish, or keep a host intercepted.
	a.completeRepeaterCapture(f)
	if a.repeaterMustIntercept("one.invalid") {
		t.Fatal("canceled lease leaked")
	}
}

func TestRepeaterHTTPSLeasePreventsPassthroughHeaderLeak(t *testing.T) {
	for _, targetKind := range []string{"IP", "DNS"} {
		t.Run(targetKind, func(t *testing.T) {
			seen := make(chan string, 1)
			target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Get(RepeaterCaptureHeader)
				_, _ = w.Write([]byte("synthetic HTTPS evidence"))
			}))
			defer target.Close()
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			_ = ln.Close()
			tr, err := Open(t.TempDir(), addr)
			if err != nil {
				t.Fatal(err)
			}
			stopped := make(chan error, 1)
			go func() { stopped <- tr.Start() }()
			t.Cleanup(func() {
				_ = tr.Close()
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Error("proxy listener not stopped")
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for {
				c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
				if err == nil {
					c.Close()
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("proxy not ready")
				case <-time.After(10 * time.Millisecond):
				}
			}
			u, _ := url.Parse(target.URL)
			if targetKind == "DNS" {
				u.Host = net.JoinHostPort("localhost", u.Port())
			}
			targetURL := u.String()
			ticket := tr.NewRepeaterCapture(u.Hostname())
			defer ticket.Close()
			tr.pass.Store(u.Hostname(), struct{}{}) // Passthrough flips after lease acquisition.
			pem, err := os.ReadFile(tr.CACertPath())
			if err != nil {
				t.Fatal(err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(pem) {
				t.Fatal("missing recorder CA")
			}
			proxy, _ := url.Parse(tr.ProxyAddr())
			transport := &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, DisableKeepAlives: true}
			defer transport.CloseIdleConnections()
			req, _ := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
			req.Header.Set(RepeaterCaptureHeader, ticket.Token())
			resp, err := (&http.Client{Transport: transport}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(data) != "synthetic HTTPS evidence" {
				t.Fatalf("TLS path failed: %q %v", data, err)
			}
			if value := <-seen; value != "" {
				t.Fatal("private marker reached HTTPS target")
			}
			result := ticket.Wait(ctx)
			if result.TrafficID == "" {
				t.Fatalf("forced intercepted response not recorded: %+v", result)
			}
			ticket.Close()
			if tr.repeaterMustIntercept(u.Hostname()) {
				t.Fatal("finished HTTPS request leaked lease")
			}
		})
	}
}
