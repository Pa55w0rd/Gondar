# Gondar · 一个不太靠谱的实验分支

Gondar 是从 **ARTEX 开源代码二次开发**而来的个人实验项目。原始公开代码作者、版权、后续贡献者和依赖许可保留在 [NOTICE.md](NOTICE.md) 与 [LICENSE](LICENSE) 中。本项目不是 ARTEX 官方版本，也不代表获得了其商业授权或安全背书。

说直白一点：这是在原项目基础上，让 **Codex 参与“乱改一通”** 后继续修修补补的分支。大量改动由 AI 辅助完成，没有经过充分人工审计和完整真实场景验证。**代码可能存在安全漏洞、逻辑错误和不兼容问题，使用有风险。** 能启动、测试通过或扫描出了结果，都不代表它安全可靠。

本项目从 **0.1** 重新编号。这是版本命名调整，不是回滚代码或删除此前的修改。公开仓库以源码快照建立首次提交，之前的维护历史与标签保存在本地；首次提交的作者身份不改变上游代码的版权归属。

当前版本为 **0.3.2**（构建 `0.3.2`）：修复局部分支受阻就过早等待的问题，增加AI自动推进回复和有依据的新目标发现，让新证据与可执行意图优先，保留既有成果、防空转、授权边界和故障保护。详见 [更新说明](RELEASE_NOTES.md)。

## 来源与版权

依据上游保留的许可及微步关于 ARTEX 的版权声明，本项目继续按 **AGPL-3.0** 使用和修改相关代码：保留原作者版权与许可声明，随分发附完整许可文本，并注明本分支的修改。产品改名不改变原代码的归属。

分发修改后的程序、镜像或向用户提供修改版网络服务时，应按适用条款提供**实际使用版本的对应源码**及必要构建材料。不能只给原版源码，也不能用免责声明代替许可义务。涉及闭源商业产品或其他超出当前许可的用途，须向有权授权的权利人核实另行授权；本仓库不承诺已经取得此类授权。具体以 [AGPL-3.0 原文](https://www.gnu.org/licenses/agpl-3.0.en.html)及相关权利人的正式声明为准。

上游后续闭源的功能不在本项目的授权或能力承诺之内。公开二开版本需自行核对来源，勿把非官方镜像、Fork名称或所谓漏洞战绩当作可信证明。

## 0.1 主要改动

以下是对已有改动的归纳，并不表示所有功能都已充分验证：

- **任务与场景**：保留任务规划和多执行者协作，提供 Web/API、登录态业务、资产梳理、漏洞复测、CTF 及批量域名/IP 场景，支持人工跟进和有限计划任务。
- **执行稳定性**：调整超时预算、失败/空转控制、请求限频与并发，修复终态任务跟进被旧预算吞掉等问题。
- **证据与报告**：增加附件预览、图片缩放、人工归属建议、复测版本保护、Markdown及Word批量导出，补齐报告缺口提示。
- **工具与模型**：整理技能和MCP配置入口，调整模型窗口说明；Docker内接入工具安装助手及版本检测。
- **宿主环境页**：按实际当前版本与最新版本显示状态，缺失时提供安装，确认有更新时才提供更新。
- **范围与数据**：修复资产查询OR组合的范围约束及工作区目录链接问题，补充出口检测、经验审核、资源与清理记录。

这些改动仍可能引入新的回归。场景提示词、审批和局部范围校验不是覆盖所有命令、MCP或网络出口的强制沙箱；清理记录与归档也不等于数据零留存。

## Docker 部署

Docker Hub 镜像为 [pa55w0rd01/gondar](https://hub.docker.com/r/pa55w0rd01/gondar)，当前版本使用 `0.3.2` 标签，`latest` 指向最新发行版。实际平台与固定镜像摘要见 [Releases](https://github.com/Pa55w0rd/Gondar/releases/tag/v0.3.2)。

```bash
git clone https://github.com/Pa55w0rd/Gondar.git
cd Gondar
cp .env.example .env
# 编辑 .env，设置自己的 POSTGRES_PASSWORD；模型可以进入页面后配置。
docker compose pull
docker compose up -d
```

默认页面为 [http://127.0.0.1:8787](http://127.0.0.1:8787)，代理端口为 `8788`，仅绑定本机；已有本地独立部署使用 `18787 / 18788`，以实际端口映射为准。首次访问设置密码，用户名 `Gondar`。需要对外服务时自行配置反向代理和访问控制。

数据库、`data`和`tools`卷用于持久化，停止服务可执行`docker compose stop`；备份时同时保留数据库、数据目录、工具卷和私有`.env`，不要用`down -v`删除数据。容器中的“宿主环境”指当前容器，安装不会自动装到物理宿主机；APT系统包重建后的保留情况需另核。

## install.sh 安装入口

`install.sh`支持Linux上的二进制与Docker两种部署形态，默认使用本项目的GitHub Releases和固定发行公钥，支持`x86_64 / aarch64`。Windows上的Docker Desktop部署请使用前面的Compose方式。

先从仓库取得并审阅脚本，也可以先只下载、校验发行包：

```bash
# 不部署、不修改系统配置；需要 curl、OpenSSL、unzip 等基础命令。
bash ./install.sh --verify-only --version=latest -y

# 二进制模式：安装程序、PostgreSQL和systemd服务，首次启动后在页面设置密码。
sudo bash ./install.sh --mode=binary --version=v0.3.2 -y

# Docker模式：使用Releases部署说明中公布并核对过的完整镜像摘要。
sudo bash ./install.sh --mode=docker --version=v0.3.2 \
  --image='pa55w0rd01/gondar@sha256:在此填入Releases公布的完整摘要' -y
```

实际部署需要root权限。可用`--dir=/opt/gondar`指定目录，用`--install-proxy=http://127.0.0.1:7890`指定安装期代理；全部选项见`bash ./install.sh --help`。二进制和Docker模式都会验证发行签名与安装包摘要，Docker模式还要求明确的`image@sha256`引用；普通Compose部署可直接使用前面的版本标签配置。

重复安装会替换程序并保留已有数据与配置，升级前仍应备份。`--with-tools`只补系统源工具，其他工具可进入宿主环境页检测后安装；可用且没有确认更新的工具不会重装。脚本仅校验模式的通过，不等于已经在你的实际宿主上验证了完整systemd、数据库或Docker初始化流程。

## 在线更新与发行

应用内检查、下载和安装更新统一使用 [Pa55w0rd/Gondar 的 GitHub Releases](https://github.com/Pa55w0rd/Gondar/releases)。主程序与安装助手均固定发行仓库和Ed25519公钥，先验证`SHA256SUMS.sig`，再核对对应安装包的SHA256；不会接受下载包临时提供的替代公钥。公共API限流时改查同仓库的官方发行页，并验证签名清单后识别安装包。

正式版本标签使用`v0.1.0`这类格式，推送新标签后由GitHub Actions构建发行包并签名。发行公钥保存在 [release-public-key.txt](deployment/release-public-key.txt)，私钥保存在维护者本机及仓库的Actions Secret中，不随源码、镜像或发行包分发。后续维护自己的分支时，应替换为自己的发行仓库及密钥，并配置`GONDAR_RELEASE_SIGNING_KEY` Secret与`GONDAR_RELEASE_PUBLIC_KEY` Variable。

Docker中使用应用内更新时，程序替换发生在当前容器可写层。以后重建容器会使用镜像自带版本，因此需要同步选择相应版本的镜像；修改`.env`中的`GONDAR_TAG`后执行`docker compose pull && docker compose up -d`。镜像拉取与应用内Releases更新是两个入口。

## 从源码构建

需要 PostgreSQL、Go 和 Node.js，具体依赖以仓库清单为准。正式构建必须嵌入前端：

```bash
git clone https://github.com/Pa55w0rd/Gondar.git
cd Gondar
GONDAR_BUILD_VERSION=0.3.2 GONDAR_OUTPUT=dist/amd64/gondar ./build.sh --target linux/amd64
docker build -t gondar:local .
cp .env.example .env
# 将 .env 的 GONDAR_IMAGE 改为 gondar、GONDAR_TAG 改为 local，并配置数据库密码：
docker compose up -d
```

`build.sh`默认使用本仓库发行源和公钥；自定义发行可通过`GONDAR_RELEASE_REPO`与`GONDAR_RELEASE_PUBLIC_KEY`覆盖。直接`go build`得到的开发构建未配置在线更新源，需要按项目构建流程生成正式产物。

## 免责声明

本项目按现状提供，不对安全性、正确性、稳定性、适用性、漏洞覆盖或测试结果作保证。AI可能误判、漏报、重复执行、消耗大量模型额度，或产生超出预期的副作用；工具、MCP、第三方依赖和二开来源也可能存在安全问题。

只应在具有明确授权的隔离测试环境中使用。运行前自行核对目标、操作范围、模型、工具、数据留存和备份，避免将真实密钥或客户材料写入公开仓库。不要仅依赖模型提示词防止越界，也不要把自动生成的报告直接当作漏洞成立的证明。

在法律允许的范围内，维护者不对因使用本项目产生的直接或间接损失承担保证责任。此说明不免除任何人的法定义务，也不削减上游许可证赋予使用者的权利；版权与许可仍以原有声明及完整许可文本为准。
