import { downloadFilename } from "./download-filename.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("UTF-8 extended filename wins regardless of parameter order and preserves percent/plus", () => {
  const name = "报告 最终+100%.docx";
  const extended = `filename*=UTF-8''${encodeURIComponent(name)}`;
  assert.equal(downloadFilename(`attachment; filename="fallback.docx"; ${extended}`, "download"), name);
  assert.equal(downloadFilename(`attachment; ${extended}; filename="fallback.docx"`, "download"), name);
  assert.equal(downloadFilename(`attachment; FILENAME*=utf-8'zh-CN'${encodeURIComponent(name)}`, "download"), name);
});

test("quoted legacy names, malformed extended values, and empty headers have usable fallbacks", () => {
  assert.equal(downloadFilename('attachment; filename="report; final.csv"', "download"), "report; final.csv");
  assert.equal(downloadFilename(String.raw`attachment; filename="report\"final.csv"`, "download"), 'report"final.csv');
  assert.equal(downloadFilename("attachment; filename*=UTF-8''%broken; filename=report.csv", "download"), "report.csv");
  assert.equal(
    downloadFilename("attachment; filename*=ISO-8859-1''report.csv; filename=legacy.csv", "download"),
    "legacy.csv",
  );
  assert.equal(downloadFilename('attachment; filename=""', "download"), "download");
  assert.equal(downloadFilename("", "download"), "download");
});

test("decoded filename cannot inject control characters or directory components", () => {
  assert.equal(downloadFilename("attachment; filename*=UTF-8''..%2Freport%0D%0A.txt", "download"), ".._report.txt");
});
