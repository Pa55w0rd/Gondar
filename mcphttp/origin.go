package mcphttp

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// mcpHTTPOrigin compares an HTTP origin, including the effective port. Explicit
// default ports are equivalent to omitted ports; paths and queries are not origins.
func mcpHTTPOrigin(u *url.URL) (string, error) {
	if u == nil || u.User != nil || u.Opaque != "" {
		return "", fmt.Errorf("MCP endpoint must be an HTTP(S) URL without userinfo")
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	switch scheme {
	case "http":
		if port == "" {
			port = "80"
		}
	case "https":
		if port == "" {
			port = "443"
		}
	default:
		return "", fmt.Errorf("MCP endpoint must use HTTP(S)")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("MCP endpoint must include a host")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("MCP endpoint has an invalid port")
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(n)), nil
}

func sameOriginMCPEndpoint(base, endpoint *url.URL) error {
	a, err := mcpHTTPOrigin(base)
	if err != nil {
		return err
	}
	b, err := mcpHTTPOrigin(endpoint)
	if err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("mcp endpoint: cross-origin URL rejected")
	}
	return nil
}

func sameOriginMCPRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("mcp: stopped after 10 redirects")
	}
	return sameOriginMCPEndpoint(via[0].URL, req.URL)
}
