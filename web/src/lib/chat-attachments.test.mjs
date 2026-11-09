import { clipboardImageFiles } from "./chat-attachments.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("clipboard image keeps the original file and ignores text/document items", () => {
  const image = new File(["image bytes"], "image.png", { type: "image/png" });
  const pdf = new File(["document"], "report.pdf", { type: "application/pdf" });
  const images = clipboardImageFiles({
    items: [
      { kind: "string", getAsFile: () => null },
      { kind: "file", getAsFile: () => image },
      { kind: "file", getAsFile: () => pdf },
      { kind: "file", getAsFile: () => null },
    ],
    files: [image],
  });
  assert.deepEqual(images, [image]);
  assert.equal(images[0], image);
});

test("clipboard supports files-only browsers without swallowing ordinary text or spoofed filenames", () => {
  const image = new File(["image"], "blob", { type: "image/jpeg" });
  assert.deepEqual(clipboardImageFiles({ items: [], files: [image] }), [image]);
  assert.deepEqual(clipboardImageFiles({ items: [{ kind: "string", getAsFile: () => null }], files: [] }), []);
  const document = new File(["plain text"], "looks-like-image.png", { type: "text/plain" });
  assert.deepEqual(clipboardImageFiles({ items: [], files: [document] }), []);
});
