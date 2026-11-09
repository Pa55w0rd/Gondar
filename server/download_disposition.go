package server

import (
	"fmt"
	"net/url"
	"strings"
)

// attachmentDisposition keeps UTF-8 names intact through RFC 5987, with an
// ASCII fallback for older clients. Filenames cannot introduce header lines.
func attachmentDisposition(name string) string {
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		if r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name))
	if name == "" || name == "." || name == ".." {
		name = "download"
	}
	fallback := strings.Map(func(r rune) rune {
		if r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	encoded := strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, fallback, encoded)
}
