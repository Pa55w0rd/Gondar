package helper

import (
	"context"
	"io"
	"net"
	"net/url"
	"testing"
	"time"
)

// The fake proxy is loopback-only and rejects the greeting. No connection to
// the synthetic destination is made; failed SOCKS negotiation must return an
// error instead of dereferencing the nil connection returned by the dialer.
func TestGetProxyConnSOCKSHandshakeFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	served := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		header := make([]byte, 2)
		if _, err = io.ReadFull(conn, header); err == nil {
			_, err = io.CopyN(io.Discard, conn, int64(header[1]))
		}
		if err == nil {
			_, err = conn.Write([]byte{5, 255})
		}
		served <- err
	}()
	proxyURL := &url.URL{Scheme: "socks5", Host: listener.Addr().String()}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := GetProxyConn(ctx, proxyURL, "fixture.invalid:443", false)
	if conn != nil {
		conn.Close()
		t.Fatal("failed negotiation returned a connection")
	}
	if err == nil {
		t.Fatal("failed negotiation returned no error")
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("fake proxy: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("fake proxy did not finish")
	}
}
