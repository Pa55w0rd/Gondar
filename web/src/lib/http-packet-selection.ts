/** Restore selected text from the raw packet: empty display rows have no DOM text. */
export function selectedPacketText(container: HTMLElement, value: string, lines: string[]): string | null {
  const selection = window.getSelection();
  if (!selection || selection.isCollapsed || selection.rangeCount !== 1) return null;
  const range = selection.getRangeAt(0);
  if (!container.contains(range.startContainer) || !container.contains(range.endContainer)) return null;

  let start: number | null = null;
  let end: number | null = null;
  let offset = 0;
  container.querySelectorAll("[data-line] code").forEach((code, index) => {
    if (range.intersectsNode(code)) {
      const selected = document.createRange();
      selected.selectNodeContents(code);
      if (range.compareBoundaryPoints(Range.START_TO_START, selected) > 0) {
        selected.setStart(range.startContainer, range.startOffset);
      }
      if (range.compareBoundaryPoints(Range.END_TO_END, selected) < 0) {
        selected.setEnd(range.endContainer, range.endOffset);
      }
      const prefix = document.createRange();
      prefix.selectNodeContents(code);
      prefix.setEnd(selected.startContainer, selected.startOffset);
      start ??= offset + prefix.toString().length;
      prefix.setEnd(selected.endContainer, selected.endOffset);
      end = offset + prefix.toString().length;
    }
    const lineEnd = offset + lines[index].length;
    offset = lineEnd + (value[lineEnd] === "\r" ? 2 : 1);
  });
  return start !== null && end !== null ? value.slice(start, end) : null;
}
