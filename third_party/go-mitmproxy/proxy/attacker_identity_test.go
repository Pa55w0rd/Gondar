package proxy

import "testing"

func TestGondarCertificateNameUsesConnectionLocalConnectFallback(t *testing.T) {
	for _, tc := range []struct{ sni, connect, want string }{
		{"", "127.0.0.1:443", "127.0.0.1"},
		{"", "[::1]:443", "::1"},
		{"", "[2001:db8::12]:8443", "2001:db8::12"},
		{"", "localhost:443", "localhost"},
		{"", "127.0.0.1", "127.0.0.1"},
		{"", "[::1]", "::1"},
		{"fixture.invalid", "127.0.0.1:443", "fixture.invalid"},
		{"fixture.invalid", "[::1]:443", "fixture.invalid"},
	} {
		if got := certificateNameForClientHello(tc.sni, tc.connect); got != tc.want {
			t.Fatalf("sni=%q CONNECT=%q got=%q want=%q", tc.sni, tc.connect, got, tc.want)
		}
	}
}
