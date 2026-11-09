package traffic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lqqyt2423/go-mitmproxy/cert"
	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

type tlsCertificateMode struct {
	mproxy.BaseAddon
	eager bool
}

func (a *tlsCertificateMode) ClientConnected(conn *mproxy.ClientConn) {
	conn.UpstreamCert = a.eager
}

// Exercise both upstream-first and lazy MITM TLS callbacks. Clients trust only
// the generated proxy CA and keep hostname/IP verification enabled. SslInsecure
// below is solely for the proxy's connection to httptest's self-signed origin,
// matching the recorder's existing upstream policy; it never relaxes the client.
func TestRecordingProxyCertificatesMatchConnectIdentity(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Host)
	}))
	defer target.Close()
	targetURL, _ := url.Parse(target.URL)
	for _, eager := range []bool{true, false} {
		t.Run(fmt.Sprintf("upstream_first_%t", eager), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			_ = listener.Close()
			proxy, err := mproxy.NewProxy(&mproxy.Options{Addr: address, SslInsecure: true, NewCaFunc: cert.NewSelfSignCAMemory})
			if err != nil {
				t.Fatal(err)
			}
			proxy.AddAddon(&tlsCertificateMode{eager: eager})
			proxy.SetUpstreamProxy(func(*http.Request) (*url.URL, error) { return nil, nil })
			stopped := make(chan error, 1)
			go func() { stopped <- proxy.Start() }()
			t.Cleanup(func() {
				_ = proxy.Close()
				select {
				case <-stopped:
				case <-time.After(5 * time.Second):
					t.Error("proxy listener did not stop")
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			for {
				conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
				if err == nil {
					_ = conn.Close()
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("proxy did not start")
				case <-time.After(10 * time.Millisecond):
				}
			}
			roots := x509.NewCertPool()
			root := proxy.GetCertificate()
			roots.AddCert(&root)
			proxyURL, _ := url.Parse("http://" + address)
			request := func(connectHost, verifyName string) error {
				authority := net.JoinHostPort(connectHost, targetURL.Port())
				transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true,
					TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: verifyName, MinVersion: tls.VersionTLS12}}
				defer transport.CloseIdleConnections()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+authority+"/synthetic", nil)
				if err != nil {
					return err
				}
				response, err := (&http.Client{Transport: transport}).Do(req)
				if err != nil {
					return err
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil || string(body) != authority {
					return fmt.Errorf("target body=%q error=%v", body, err)
				}
				if response.TLS == nil || len(response.TLS.VerifiedChains) == 0 {
					return fmt.Errorf("TLS certificate verification was not performed")
				}
				if err := response.TLS.PeerCertificates[0].VerifyHostname(verifyName); err != nil {
					return err
				}
				return nil
			}
			cases := []struct{ name, host, verify string }{
				{"IPv4_without_SNI", "127.0.0.1", "127.0.0.1"},
				{"DNS_with_SNI", "localhost", "localhost"},
				{"SNI_takes_precedence_over_CONNECT_IP", "127.0.0.1", "localhost"},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					if err := request(tc.host, tc.verify); err != nil {
						t.Fatal(err)
					}
				})
			}
			t.Run("concurrent_IP_and_DNS_do_not_share_fallback_names", func(t *testing.T) {
				var wg sync.WaitGroup
				errors := make(chan error, 12)
				for i := 0; i < 12; i++ {
					tc := cases[i%len(cases)]
					wg.Add(1)
					go func() {
						defer wg.Done()
						if err := request(tc.host, tc.verify); err != nil {
							errors <- fmt.Errorf("%s: %w", tc.name, err)
						}
					}()
				}
				wg.Wait()
				close(errors)
				var failures []string
				for err := range errors {
					failures = append(failures, err.Error())
				}
				if len(failures) != 0 {
					t.Fatal(strings.Join(failures, "\n"))
				}
			})
		})
	}
}
