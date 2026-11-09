package proxy

import (
	"net"
	"strings"
)

// certificateNameForClientHello preserves explicit SNI. IP clients omit SNI,
// so the certificate must instead cover this connection's CONNECT target.
// Never cache a fallback name globally: concurrent connections may differ.
func certificateNameForClientHello(serverName, connectAddress string) string {
	if serverName != "" {
		return serverName
	}
	if host, _, err := net.SplitHostPort(connectAddress); err == nil {
		return host
	}
	return strings.Trim(connectAddress, "[]")
}
