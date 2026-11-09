/** Only consume actual clipboard images; text/HTML and document paste stay native. */
export function clipboardImageFiles(clipboard: Pick<DataTransfer, "items" | "files">): File[] {
  const items = Array.from(clipboard.items ?? [])
    .filter((item) => item.kind === "file")
    .map((item) => item.getAsFile())
    .filter((file): file is File => file !== null);
  const files = items.length > 0 ? items : Array.from(clipboard.files ?? []);
  return files.filter((file) => file.type.startsWith("image/"));
}
