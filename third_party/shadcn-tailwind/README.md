# shadcn/ui Tailwind 样式来源

仅保留 shadcn 4.12.0 的完整样式，未安装或执行其 CLI。上游完整提交为 `8055a12f462788f008bf5d746c9c6a585f552b29`，标签 `shadcn@4.12.0`；原 MIT 版权与许可见 `LICENSE`。

- 原始源码：<https://github.com/shadcn-ui/ui/blob/8055a12f462788f008bf5d746c9c6a585f552b29/packages/shadcn/src/tailwind.css>
- 本地文件：`web/src/styles/vendor/shadcn-tailwind.css`；注释之后的 16041 字节正文与原包及该提交源码相同。
- 来源、原包 SHA512 SRI 和正文 SHA256：`web/src/styles/vendor/shadcn-tailwind.source.json`。
- 维护过程：固定新版本与完整提交，从官方包校验完整性并核对源码；替换完整正文和元数据，保留许可，然后运行根目录 `scripts/check-vendored-styles.mjs` 与前端正式静态构建。不要通过重新安装整个 CLI 获取样式。

来源 CSS 的用途包括状态变体、折叠动画、滚动渐变遮罩、RTL 与减少动画的适配。只删除 import 或以无样式的 `@shadcn/react` 包替代，会丢失这些样式契约。
