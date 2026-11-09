# Gondar 本地维护差异

上游：<https://github.com/lqqyt2423/go-mitmproxy>，v1.9.2，完整提交 `5ae87ab47229f17f7e3b7e619b28236bb56a9114`。原 MIT 许可与版权完整保留在 `LICENSE`；不修改原 Go 模块缓存。

2026-11-04：Gondar 对 `proxy/attacker.go` 的 upstream-first 与 lazy 两个客户端 TLS 证书回调作同一修复：SNI 非空时保持原行为；SNI 为空时用当前 CONNECT authority 提取的主机名生成证书，使 IP 目标获得正确 IP SAN。提取逻辑在新增 `certificate_identity.go`，没有全局共享 fallback 名称，也不修改 ClientHello 或任何证书验证选项。

新增 `attacker_identity_test.go` 覆盖 IPv4、IPv6、DNS 与显式 SNI 优先级；Gondar 主仓的 `traffic/tls_identity_test.go` 用受控回环 HTTPS 与仅信任代理 CA 的严格客户端，验证两条真实 TLS 路径和混合并发。上游其他功能、测试和资源按 v1.9.2 保留，本轮未自动运行可能包含外部访问的上游全测试。

原始 104 个文件的 SHA256 与修改范围见 Gondar 的 `gondar_change/1.3/mitmproxy-provenance.json`。来源 `AGENTS.md` 作为资料改存 `UPSTREAM_AGENTS.txt`，不会成为 Gondar 的协作指令。移除本地 replace 前，应在上游正式修复版本上运行同一回归，不能仅凭版本号删除补丁。
