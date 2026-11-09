import assert from "node:assert/strict";
import test from "node:test";
import { MCP_PRESETS, parseMCPArguments } from "./mcp-presets.ts";

test("MCP presets never contain rolling downloads, sandbox bypass or task-wide filesystem defaults", () => {
  for (const p of MCP_PRESETS) {
    assert.ok(!["npx", "uvx", "curl", "sh"].includes(p.command));
    assert.ok(!p.args.some((a) => a.includes("@latest") || a === "--no-sandbox" || a === "/app/data"));
  }
  assert.deepEqual(MCP_PRESETS.find((p) => p.id === "filesystem").args, ["<填写授权目录>"]);
});

test("argument arrays preserve whitespace and literal shell syntax without evaluation", () => {
  const args = ["C:\\授权目录\\two words", "$(not-a-command)", "literal; value", "含中文"];
  assert.deepEqual(parseMCPArguments(JSON.stringify(args)), args);
  assert.deepEqual(parseMCPArguments("--headless  --isolated"), ["--headless", "--isolated"]);
  assert.throws(() => parseMCPArguments('["ok", 3]'));
  assert.throws(() => parseMCPArguments('["bad\\u0000"]'));
});
