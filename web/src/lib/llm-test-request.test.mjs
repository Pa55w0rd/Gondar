import { llmTestRequest } from "./llm-test-request.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("仅profile保留省略，明确false与空值不被补默认", () => {
  assert.deepEqual(llmTestRequest({ profile_id: 9 }), { profile_id: 9 });
  assert.deepEqual(llmTestRequest({ profile_id: 9, streaming: false, proxy: "", max_tokens: 0 }), {
    profile_id: 9,
    streaming: false,
    proxy: "",
    max_tokens: 0,
  });
});

test("原位置参数调用仍保留原收发默认和空密钥语义", () => {
  const value = llmTestRequest(
    "openai",
    "fixture",
    "https://fixture.invalid/v1",
    "",
    "",
    "",
    "",
    9,
    false,
    "X-Fixture",
  );
  assert.equal(value.streaming, false);
  assert.equal(value.api_key, "");
  assert.equal(value.profile_id, 9);
  assert.equal(value.session_header_key, "X-Fixture");
  assert.equal(llmTestRequest("openai", "fixture", "", "").streaming, true);
});
