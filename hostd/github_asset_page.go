package hostd

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// A digest must belong to the SAME list item as the exact repository/tag/asset
// link. A checksum elsewhere on the page cannot authorise another executable.
func expandedAssetDigest(raw []byte, expectedPath string) (string, error) {
	root, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	result := ""
	conflict := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "li" {
			matched, links := false, 0
			digests := []string{}
			var scan func(*html.Node)
			scan = func(item *html.Node) {
				for _, a := range item.Attr {
					if item.Data == "a" && a.Key == "href" {
						u, e := url.Parse(a.Val)
						if e == nil && (u.Host == "" && u.Scheme == "" || u.Scheme == "https" && u.Host == "github.com" && u.User == nil) && strings.Contains(u.Path, "/releases/download/") {
							links++
							if u.Path == expectedPath && u.RawQuery == "" && u.Fragment == "" {
								matched = true
							}
						}
					}
					if item.Data == "clipboard-copy" && a.Key == "value" && strings.HasPrefix(a.Val, "sha256:") {
						d := strings.TrimPrefix(a.Val, "sha256:")
						if digestPattern.MatchString(d) {
							digests = append(digests, strings.ToLower(d))
						}
					}
				}
				for c := item.FirstChild; c != nil; c = c.NextSibling {
					scan(c)
				}
			}
			scan(n)
			if matched && links == 1 && len(digests) == 1 {
				if result != "" && result != digests[0] {
					conflict = true
				}
				result = digests[0]
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	if result == "" || conflict {
		return "", fmt.Errorf("官方资产页没有该精确安装包的唯一 SHA256")
	}
	return result, nil
}
