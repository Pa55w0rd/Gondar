# 来源、版权与许可

Gondar 从 ARTEX 开源代码二次开发而来，原始公开代码主要作者为 **Autumn-27**。微步关于 ARTEX 的版权声明及代码已有的版权、许可义务不因本项目改名或重新建立提交历史而改变。

本项目采用 [GNU AGPL-3.0](LICENSE)。原有源文件的版权与许可声明继续保留，第三方代码和资源同时保留各自的权利归属及许可证。维护者 Pa55w0rd 的首次提交只表示整理、修改和发布当前源码快照，不表示独立创作了全部代码。

## 代码来源与历史贡献

本公开仓库于2026-11-09以当前源码快照建立首次提交。此前维护历史和详细比对记录留在本地，不作为公开仓库链接或源码交付的替代品。主要来源锚点如下：

| 来源 | 固定版本与提交 | 承接情况 |
|---|---|---|
| ARTEX，原作者 Autumn-27 | 原始公开版 v0.3.15，`e6ec56999175509551a1e43a3ee3b053d0941bd9` | 本项目的原始代码基础；原项目地址为 `https://github.com/Autumn-27/ARTEX` |
| Gondar此前维护快照 | `9ece962ae089e0efeed9835efa0a1ce23fc52707`，维护编号0.1 / 构建0.1.0 | 本次公开快照的本地起点；主要改动和风险见[README](README.md) |

已保留的承接历史包含以下署名：Autumn、Autumn-27、begininvoke、dingpotian、FileShare Developer、ji ruo、lornlee、luckone、mhtsec、Pa55w0rd、pphua、RuoJi6、xingchuan、榆木。此处是历史贡献归属，不是本公开仓库的提交者列表，也不表示他们对本分支作出背书。

其他来源的模块改写与适配包括：

| 来源与固定提交 | 涉及内容 | 许可与处理 |
|---|---|---|
| [BRTEX 59ba84ca](https://github.com/ThanatosXingYu/BRTEX/commit/59ba84ca9ca8fc2d4a6ffce07ed5f969b8d0e074) | 模型配置与MCP结果处理 | AGPL-3.0，适配到现有入口 |
| [reArtex 4445a188](https://github.com/wufufu770/reArtex/commit/4445a188e6bfa968a16047c0f284e9b33135cf45) | legacy SSE连接的origin约束 | AGPL-3.0，改写归一化与重定向校验 |
| [artex-ko-main ad4277ac](https://github.com/azrielnan/artex-ko-main/commit/ad4277ac2d6a3dc346a7c88097aaad1b5b28d9dd)、[4532d79b](https://github.com/azrielnan/artex-ko-main/commit/4532d79bc4f934435af378c250b92954532c1067) | 截图及技能状态 | AGPL-3.0，沿用本地上传与白名单机制 |
| [MoLeft/ARTEX 542d7e9c](https://github.com/MoLeft/ARTEX/commit/542d7e9ca63835bf830ba0a8ed9c838da7177224) | 下载文件名兼容处理 | AGPL-3.0，统一本地解析与出口 |
| [RuoJi6/ARTEX b9b137dc](https://github.com/RuoJi6/ARTEX/commit/b9b137dc914139194a6f0debfb2d1552c79bf525) | 原始报文复制 | AGPL-3.0，按现有DOM与报文格式适配 |
| [Pojun](https://github.com/chu0119/Pojun) | 按模块吸收适用改动 | AGPL-3.0，适配部分模块 |
| [ARTEX-main 5ce2fc9d](https://github.com/sharenjun/ARTEX-main/commit/5ce2fc9d670180711a577ad07baca755a5922691) | 工具调用与结果配对 | AGPL-3.0，保留本地游标及NULL兼容 |

上表用于保留主要来源归属，不能替代具体文件自身的许可证。没有整仓复制的执行器、商业闭源功能或宣传能力，不因列出项目名称而视为本项目已经获得授权或具备该能力。

## 保留的第三方代码与资源

- Norma v0.4.4，Apache-2.0；固定源码副本及原始逐文件摘要保留在 [third_party/norma](third_party/norma/)。本地维护改动见 [GONDAR_NOTICE.md](third_party/norma/GONDAR_NOTICE.md)，完整许可随源码、发行包与镜像提供，见 [LICENSE](third_party/norma/LICENSE)。
- [go-mitmproxy v1.9.2](https://github.com/lqqyt2423/go-mitmproxy)，作者 liqiang，MIT；来源提交 `5ae87ab47229f17f7e3b7e619b28236bb56a9114`。本地副本与修改见 [GONDAR_CHANGES.md](third_party/go-mitmproxy/GONDAR_CHANGES.md)，完整版权与许可见[第三方LICENSE](third_party/go-mitmproxy/LICENSE)。
- [shadcn/ui](https://github.com/shadcn-ui/ui)，作者 shadcn，MIT；保留 shadcn 4.12.0 的完整Tailwind样式文件，固定提交 `8055a12f462788f008bf5d746c9c6a585f552b29`。[来源与完整性记录](web/src/styles/vendor/shadcn-tailwind.source.json)、[原始许可](third_party/shadcn-tailwind/LICENSE)均随源码提供。
- 其他依赖版本见 [go.mod](go.mod)、[go.sum](go.sum)、[package.json](web/package.json)及[前端依赖锁](web/package-lock.json)。依赖、字体与其他非代码资源的原有权利归属不因仓库整体使用AGPL而改变。

分发修改版程序、镜像或提供修改版网络服务时，仍须按AGPL适用条款向接收者或远程用户提供实际运行版本的对应源码及必要构建材料。本仓库的[公开源码](https://github.com/Pa55w0rd/Gondar)与许可证可用于相应交付，但不自动替代其他部署版本所需的源码，也不代表已取得额外商业授权。
