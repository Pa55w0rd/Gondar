package agent

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Model requests can carry custom credentials (Anthropic x-api-key), session
// headers and private prompts. net/http's built-in Authorization stripping is
// insufficient; only a redirect within the original HTTP origin may receive them.
func sameOriginProviderRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return fmt.Errorf("llm: missing initial redirect request")
	}
	if len(via) >= 10 {
		return fmt.Errorf("llm: stopped after 10 redirects")
	}
	initial, err := providerHTTPOrigin(via[0].URL)
	if err != nil {
		return err
	}
	next, err := providerHTTPOrigin(req.URL)
	if err != nil {
		return err
	}
	if initial != next {
		return fmt.Errorf("llm: cross-origin redirect rejected")
	}
	return nil
}

func providerHTTPOrigin(u *url.URL) (string, error) {
	if u == nil || u.User != nil || u.Opaque != "" {
		return "", fmt.Errorf("llm: redirect requires an HTTP(S) URL without userinfo")
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
		return "", fmt.Errorf("llm: redirect requires HTTP(S)")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("llm: redirect requires a host")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("llm: redirect has an invalid port")
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(n)), nil
}
