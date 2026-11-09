package db

import (
	"net/url"
	"strings"
)

type AssetRoleHint struct {
	Role  string `json:"role"`
	Basis string `json:"basis"`
}

// AssetRoleHints are navigation hints from observed names/paths, not verified
// service identity or authorization. InScope and Tested are never derived here.
func AssetRoleHints(host, rawURL string) []AssetRoleHint {
	var hints []AssetRoleHint
	add := func(role, basis string) {
		for _, h := range hints {
			if h.Role == role {
				return
			}
		}
		hints = append(hints, AssetRoleHint{Role: role, Basis: basis})
	}
	u, err := url.Parse(rawURL)
	if err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	first := strings.Split(strings.ToLower(strings.TrimSuffix(host, ".")), ".")[0]
	switch first {
	case "admin", "console", "manage", "manager":
		add("管理入口", "主机首段名称为 "+first)
	case "auth", "login", "sso", "passport":
		add("认证入口", "主机首段名称为 "+first)
	case "api", "graphql":
		add("接口服务", "主机首段名称为 "+first)
	case "oss", "s3", "minio", "storage":
		add("对象存储", "主机首段名称为 "+first)
	case "static", "cdn", "assets":
		add("静态资源", "主机首段名称为 "+first)
	}
	if err == nil {
		for _, segment := range strings.Split(strings.ToLower(u.Path), "/") {
			switch segment {
			case "login", "signin", "oauth", "sso":
				add("认证入口", "路径包含完整段 "+segment)
			case "admin", "console", "management":
				add("管理入口", "路径包含完整段 "+segment)
			case "api", "graphql", "swagger", "openapi.json":
				add("接口服务", "路径包含完整段 "+segment)
			}
		}
	}
	return hints
}
