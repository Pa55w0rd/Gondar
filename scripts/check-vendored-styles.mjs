import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFile } from "node:fs/promises";
import { createRequire } from "node:module";
import path from "node:path";
import { fileURLToPath } from "node:url";

const webDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../web");
const require = createRequire(path.join(webDir, "package.json"));
const postcss = require("postcss");
const tailwind = require("@tailwindcss/postcss");
const vendorDir = path.join(webDir, "src/styles/vendor");
const source = JSON.parse(await readFile(path.join(vendorDir, "shadcn-tailwind.source.json"), "utf8"));
const normalize = (text) => text.replace(/\r\n/g, "\n");
const sha256 = (text) => createHash("sha256").update(text).digest("hex");
const vendor = normalize(await readFile(path.join(vendorDir, "shadcn-tailwind.css"), "utf8"));
const markerIndex = vendor.indexOf(source.body_marker);
assert.notEqual(markerIndex, -1, "缺少上游样式正文边界");
const body = vendor.slice(markerIndex + source.body_marker.length);
assert.equal(sha256(body), source.upstream_css_sha256, "固定版本的样式正文不完整或已改变；请复核来源记录");
const license = normalize(await readFile(path.resolve(vendorDir, source.license_path), "utf8"));
assert.equal(sha256(license), source.license_sha256, "原始 MIT 许可不匹配");

const args = process.argv.slice(2);
assert.ok(args.length === 0 || (args.length === 2 && args[0] === "--upstream"), "只接受可选的 --upstream 本地路径");
const upstream = args.length === 2 ? normalize(await readFile(path.resolve(args[1]), "utf8")) : body;
assert.equal(sha256(upstream), source.upstream_css_sha256, "对照文件不是已固定的原始 CSS");

// Compare real Tailwind output, including state selectors, scrolling masks and
// motion fallbacks; merely resolving an import would not preserve this contract.
const candidates = [
  "data-open:opacity-100", "data-closed:opacity-0", "data-checked:bg-red-500",
  "data-unchecked:bg-blue-500", "data-selected:text-red-500", "data-disabled:opacity-50",
  "data-active:font-bold", "data-horizontal:flex-row", "data-vertical:flex-col",
  "no-scrollbar", "scroll-fade", "scroll-fade-x", "scroll-fade-y", "scroll-fade-t",
  "scroll-fade-b", "scroll-fade-l", "scroll-fade-r", "scroll-fade-s", "scroll-fade-e",
  "scroll-fade-none", "scroll-fade-4", "shimmer", "shimmer-once", "shimmer-reverse",
  "shimmer-none", "shimmer-color-red-500", "shimmer-duration-1000", "shimmer-angle-20",
];
async function compile(css, name) {
  const input = `@import "tailwindcss" source(none);\n@source inline(${JSON.stringify(candidates.join(" "))});\n${css}`;
  return (await postcss([tailwind({ base: webDir, optimize: true })]).process(input, {
    from: path.join(webDir, `${name}.css`), map: false,
  })).css;
}
function canonical(css) {
  const root = postcss.parse(css);
  root.walkComments((comment) => comment.remove());
  return JSON.stringify(root.toJSON(), (key, value) => ["raws", "source", "inputs"].includes(key) ? undefined : value);
}
const before = await compile(upstream, "upstream-styles-fixture");
const after = await compile(vendor, "vendored-styles-fixture");
assert.equal(canonical(after), canonical(before), "本地样式与原始样式的真实 Tailwind 编译结果不同");
for (const token of ["data-open", "data-closed", "data-checked", "data-unchecked", "data-selected", "data-disabled", "data-active", "data-horizontal", "data-vertical", "no-scrollbar", "scroll-fade", "shimmer", "prefers-reduced-motion", "mask-image"]) {
  assert.ok(after.includes(token), `编译产物缺少样式契约：${token}`);
}
console.log(JSON.stringify({
  stylesheet: "shadcn@4.12.0", commit: source.commit, candidates: candidates.length,
  upstream_css_sha256: source.upstream_css_sha256, compiled_css_bytes: Buffer.byteLength(after),
  equivalent_compiled_ast: true, external_css_baseline: args.length === 2,
  cli_dependency_installed: false,
}, null, 2));
