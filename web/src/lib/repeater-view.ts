// Import helpers carry only an origin and a recorder ID in the URL. Request
// credentials remain in the authenticated exchange API and task-owned history.
export function repeaterImportHref(id: string, rawURL: string): string | null {
  try {
    const url = new URL(rawURL);
    if (url.protocol !== "http:" && url.protocol !== "https:") return null;
    return `/function/repeater?${new URLSearchParams({ source: id, origin: url.origin })}`;
  } catch {
    return null;
  }
}

export function repeaterRequestWithHost(raw: string, origin: string): string {
  if (!raw.trim() || /^host\s*:/im.test(raw)) return raw;
  const host = new URL(origin).host;
  const newline = raw.includes("\r\n") ? "\r\n" : "\n";
  const end = raw.indexOf(newline);
  return end < 0
    ? `${raw}${newline}Host: ${host}${newline}${newline}`
    : `${raw.slice(0, end + newline.length)}Host: ${host}${newline}${raw.slice(end + newline.length)}`;
}

export function repeaterBodyText(base64: string | undefined, headers = ""): string {
  return repeaterBodyView(base64, headers).text;
}

export function repeaterBodyView(base64: string | undefined, headers = ""): { text: string; comparable: boolean } {
  if (!base64) return { text: "", comparable: true };
  // Responses are stored as bytes. Do not display compressed data as if it was
  // the uncompressed application response, or interpret returned HTML.
  const encoding = /^content-encoding:\s*([^\r\n]+)/im.exec(headers)?.[1]?.trim().toLowerCase();
  if (encoding && encoding !== "identity")
    return { text: `[${encoding} 编码的原始字节；请移除请求中的 Accept-Encoding 后重发以查看文本]`, comparable: false };
  try {
    const bytes = Uint8Array.from(atob(base64), (c) => c.charCodeAt(0));
    if (bytes.includes(0)) return { text: `[二进制正文，${bytes.length} 字节；历史保留原始字节]`, comparable: false };
    return { text: new TextDecoder("utf-8", { fatal: true }).decode(bytes), comparable: true };
  } catch {
    return { text: "[正文无法按 UTF-8 文本显示；历史保留原始字节]", comparable: false };
  }
}

// A bounded line comparison, not an O(n*m) diff over arbitrary remote content.
// Status, byte length and SHA-256 remain the authoritative comparison summary.
export function repeaterLineChanges(before: string, after: string): string {
  const a = before.split(/\r?\n/);
  const b = after.split(/\r?\n/);
  const limit = Math.min(Math.max(a.length, b.length), 2000);
  const changed: string[] = [];
  for (let i = 0; i < limit; i++) {
    if (a[i] === b[i]) continue;
    changed.push(`第 ${i + 1} 行`, `− ${(a[i] ?? "∅").slice(0, 500)}`, `+ ${(b[i] ?? "∅").slice(0, 500)}`);
    if (changed.length >= 240) break;
  }
  if (!changed.length && before === after) return "文本相同";
  if (!changed.length) return "前 2000 行相同，后续内容不同；请核对长度与摘要";
  return (
    changed.join("\n") +
    (changed.length >= 240 || Math.max(a.length, b.length) > 2000 ? "\n[仅显示前 80 处差异 / 前 2000 行]" : "")
  );
}
