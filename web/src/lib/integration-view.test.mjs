import { formatIntegrationTime } from "./integration-view.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("valid IANA timezone keeps its local clock", () => {
  assert.match(formatIntegrationTime("2026-11-04T01:00:00Z", "Asia/Shanghai"), /09:00:00/);
});
test("damaged legacy timezone falls back to labelled UTC", () => {
  assert.match(formatIntegrationTime("2026-11-04T01:00:00Z", "Local"), /01:00:00.*UTC/);
});
test("invalid timestamp is explicitly labelled", () => {
  assert.equal(formatIntegrationTime("invalid", "Asia/Shanghai"), "时间无效");
});
