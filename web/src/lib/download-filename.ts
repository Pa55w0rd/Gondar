function usableDownloadName(name: string, fallback: string): string {
  // biome-ignore lint/suspicious/noControlCharactersInRegex: decoded headers must not retain control characters.
  const withoutControls = name.replace(/[\u0000-\u001f\u007f]/g, "");
  const clean = withoutControls.replace(/[/\\]/g, "_").trim();
  return clean && clean !== "." && clean !== ".." ? clean : fallback;
}

/** Prefer RFC 5987 UTF-8 names, falling back to legacy filenames when malformed. */
export function downloadFilename(contentDisposition: string, fallback: string): string {
  const extended = contentDisposition.match(/(?:^|;)\s*filename\*\s*=\s*(?:"([^"]*)"|([^;]*))/i);
  const encoded = (extended?.[1] ?? extended?.[2] ?? "").trim().match(/^UTF-8'[^']*'(.*)$/i)?.[1];
  if (encoded) {
    try {
      const name = usableDownloadName(decodeURIComponent(encoded), "");
      if (name) return name;
    } catch {
      // An invalid percent/UTF-8 sequence must not break a successful download.
    }
  }
  const legacy = contentDisposition.match(/(?:^|;)\s*filename\s*=\s*(?:"((?:\\.|[^"\\])*)"|([^;]*))/i);
  const name = legacy?.[1] !== undefined ? legacy[1].replace(/\\(.)/g, "$1") : (legacy?.[2] ?? "");
  return usableDownloadName(name, fallback);
}
