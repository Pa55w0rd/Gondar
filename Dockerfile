# syntax=docker/dockerfile:1
#
# 运行镜像（不在镜像里编译）：只装常用工具，放入**预编译好的 Linux 单二进制**。
# 二进制由 CI 的 binaries job 交叉编译（纯 Go、无 QEMU），按目标架构放在
# 构建上下文的 dist/<TARGETARCH>/gondar。这样多架构构建时 arm64 只需模拟 apt 层，
# 不再模拟 Next/Go 编译，速度快得多。
#
# 本地手动构建镜像时，先自行准备二进制：
#   cd web && npm run build:static && cd ..
#   cp -r web/out server/webui/dist
#   CGO_ENABLED=0 GOARCH=amd64 go build -tags embedui -o dist/amd64/gondar ./cmd/gondar
#   CGO_ENABLED=0 GOARCH=amd64 go build -o dist/amd64/gondar-hostd ./cmd/gondar-hostd
#   docker build -t gondar:local .
FROM python:3.12-slim-bookworm
ARG TARGETARCH
# 常用工具：ripgrep / curl / vim，加一批 recon 常备件（按需增删）。
# 系统 CA 使用 eMudhra 事件后的修复版本；Node 从官方归档按固定摘要安装，
# 不再执行滚动的 NodeSource 远端 shell 脚本。版本与摘要证据见 1.3 部署记录。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip xz-utils \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
      imagemagick libreoffice-writer libreoffice-calc libreoffice-impress poppler-utils \
    && dpkg --compare-versions "$(dpkg-query -W -f='${Version}' ca-certificates)" ge '20250419~deb12u2' \
    && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) node_arch="x64"; node_sha="1805bcd35e6815559bd2eb97db438d3fdb0eb3a5e6a977b7c1badca76337e9ce" ;; \
      arm64) node_arch="arm64"; node_sha="6287aa46b6c375d816e63ae66d024fbbfd70404825aabdfdf898a970871d2f4b" ;; \
      *) echo "unsupported arch: $TARGETARCH"; exit 1 ;; \
    esac; \
    curl -fsSL --proto '=https' --proto-redir '=https' --max-filesize 134217728 \
      "https://nodejs.org/dist/v22.23.4/node-v22.23.4-linux-${node_arch}.tar.xz" -o /tmp/node.tar.xz; \
    echo "$node_sha  /tmp/node.tar.xz" | sha256sum -c -; \
    tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1; \
    test "$(node --version)" = "v22.23.4"; \
    rm -f /tmp/node.tar.xz
# nuclei(漏洞模板扫描引擎,配合 /app/nuclei-templates 情报模板库):
# 官方 release 单二进制按架构下载,模板树由 gondar 的 vulnintel 同步器维护
# (git clone --depth 1 到 /app/nuclei-templates,每轮同步 git pull 更新)。
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) nuclei_arch="linux_amd64" ;; \
      arm64) nuclei_arch="linux_arm64" ;; \
      *) echo "unsupported arch: $arch"; exit 1 ;; \
    esac; \
    nuclei_ver="v3.11.1"; \
    case "$arch" in \
      amd64) nuclei_sha="ea63d4ae232808cd7c6bc00d0142428e231fab59dae01042246097d195835ab6" ;; \
      arm64) nuclei_sha="8044e3d9768ba0a744b2872c1a87e813006f013da97ca9f50f7661a4203bec07" ;; \
    esac; \
    curl -fsSL --proto '=https' --proto-redir '=https' --max-filesize 134217728 "https://github.com/projectdiscovery/nuclei/releases/download/${nuclei_ver}/nuclei_${nuclei_ver#v}_${nuclei_arch}.zip" -o /tmp/nuclei.zip; \
    echo "$nuclei_sha  /tmp/nuclei.zip" | sha256sum -c -; \
    unzip -o /tmp/nuclei.zip -d /tmp/nuclei && install -m 0755 /tmp/nuclei/nuclei /usr/local/bin/nuclei \
    && nuclei -version && rm -rf /tmp/nuclei /tmp/nuclei.zip
# 预装 Playwright MCP 与 CLI（全局），运行时不再 npx 联网下载。
# @playwright/mcp：browser MCP 直接 `npx @playwright/mcp`（已全局装好，无需 -y/@latest）。
# @playwright/cli：提供 playwright-cli，装完顺带 --help 验证可执行。
# 再装 playwright（提供浏览器管理），装完用 --with-deps 预置 chromium 及其系统依赖，
# 这样容器内 MCP/CLI 首次启动即可用，不再联网下载浏览器。
RUN npm install --ignore-scripts -g @playwright/mcp@0.0.84 @playwright/cli@0.1.23 playwright@1.64.1 \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
# 预编译好的对应架构二进制（dist/amd64/gondar 或 dist/arm64/gondar）
COPY dist/${TARGETARCH}/gondar /app/gondar
COPY dist/${TARGETARCH}/gondar-hostd /app/gondar-hostd
# 守护启动脚本：进程退出后按退出码决定是否重新拉起，页面一键更新靠它完成换装。
# 它同时负责把 SIGTERM 转发给 gondar —— docker stop 只把信号发给 PID 1，
# 不转发的话 gondar 收不到、做不了优雅关闭，10 秒后被 SIGKILL 硬杀。
COPY start.sh /app/start.sh
COPY deployment/container-entrypoint.sh /app/container-entrypoint.sh
RUN chmod 0755 /app/gondar /app/gondar-hostd /app/start.sh /app/container-entrypoint.sh
ENV PATH="/opt/gondar-tools/python/bin:/opt/gondar-tools/bin:${PATH}"
COPY skills/ /app/skills/
COPY LICENSE NOTICE.md /app/
COPY third_party/go-mitmproxy/LICENSE /app/third_party/go-mitmproxy/LICENSE
COPY third_party/shadcn-tailwind/LICENSE /app/third_party/shadcn-tailwind/LICENSE
# data/（SQLite + jwt.key）持久化点
VOLUME ["/app/data", "/opt/gondar-tools"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/container-entrypoint.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
