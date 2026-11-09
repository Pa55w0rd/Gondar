import {
  repeaterBodyText,
  repeaterBodyView,
  repeaterImportHref,
  repeaterLineChanges,
  repeaterRequestWithHost,
} from "./repeater-view.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("traffic import URL contains only origin and ID, never credentials/query/body", () => {
  const href = repeaterImportHref("flow-1", "https://user:secret@example.test/a?token=secret");
  assert.equal(href, "/function/repeater?source=flow-1&origin=https%3A%2F%2Fexample.test");
  assert.equal(repeaterImportHref("flow", "file:///a"), null);
  assert.equal(repeaterImportHref("flow", "invalid"), null);
});

test("legacy requests receive Host while existing Host is retained for validation", () => {
  assert.equal(
    repeaterRequestWithHost("GET / HTTP/1.1\r\n\r\n", "https://example.test:8443"),
    "GET / HTTP/1.1\r\nHost: example.test:8443\r\n\r\n",
  );
  const original = "GET / HTTP/1.1\nHost: other.test\n\n";
  assert.equal(repeaterRequestWithHost(original, "https://example.test"), original);
});

test("response text is literal UTF-8 and binary/compressed bytes are labelled", () => {
  assert.equal(repeaterBodyText(btoa("<script>alert(1)</script>")), "<script>alert(1)</script>");
  assert.match(repeaterBodyText(btoa("\0binary")), /二进制/);
  assert.equal(repeaterBodyView(btoa("\0binary")).comparable, false);
  assert.match(repeaterBodyText(btoa("raw"), "Content-Encoding: gzip\r\n"), /gzip/);
  assert.match(repeaterBodyText("////"), /无法按 UTF-8/);
});

test("line comparison is bounded and does not call two prefixes identical", () => {
  assert.equal(repeaterLineChanges("same", "same"), "文本相同");
  assert.match(repeaterLineChanges("before", "after"), /− before\n\+ after/);
  const long = Array.from({ length: 5000 }, (_, i) => `line ${i}`).join("\n");
  assert.match(repeaterLineChanges(long, `${long}\nnew`), /后续内容不同/);
  assert.ok(repeaterLineChanges(long, long.replaceAll("line", "changed")).length < 90000);
});
