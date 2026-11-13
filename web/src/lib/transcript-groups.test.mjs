import assert from "node:assert/strict";
import test from "node:test";
import { groupSteps, toolStatus } from "./transcript-groups.ts";

function step(seq, kind, run = "run", inv = "inv", extra = {}) {
  const id = {
    version: 1,
    run_id: run,
    prompt_id: "prompt",
    invocation_id: inv,
    tool_use_id: "reused",
    tool_name: "probe",
  };
  return {
    seq,
    kind,
    worker: "fixture",
    tool: "probe",
    tool_use_id: "reused",
    metadata: { tool_call_identity: id, activity_origin: "runtime" },
    ...extra,
  };
}

test("不同宿主调用不按提供商ID串账，晚回执不覆盖主结果", () => {
  const rows = [
    step(1, "tool_use", "a"),
    step(2, "tool_use", "b"),
    step(3, "tool_result", "b"),
    step(4, "tool_result", "a"),
    step(5, "tool_late_result", "a"),
  ];
  const groups = groupSteps(rows, false);
  assert.equal(groups[0].result.seq, 4);
  assert.equal(groups[1].result.seq, 3);
  assert.equal(groups[2].result.seq, 5);
});

test("重复宿主身份永久保持歧义，旧同参记录也不强行配对", () => {
  const typed = groupSteps(
    [step(1, "tool_use"), step(2, "tool_use"), step(3, "tool_use"), step(4, "tool_result")],
    false,
  );
  assert.equal(typed.length, 4);
  assert.ok(typed.slice(0, 3).every((g) => !g.result));
  const old = [step(1, "tool_use"), step(2, "tool_use"), step(3, "tool_result")].map((s) => ({ ...s, metadata: {} }));
  assert.equal(groupSteps(old, false).length, 3);
});

test("仅匹配的宿主结果区分管控拒绝，响应正文无权决定类别", () => {
  const row = step(1, "tool_result", "run", "inv", { is_error: true, detail: "403 policy_denied" });
  assert.equal(toolStatus(row).tone, "error");
  row.metadata.tool_outcome = {
    ...row.metadata.tool_call_identity,
    phase: "result",
    kind: "policy_denied",
    execution_state: "not_executed",
    source: "policy",
  };
  assert.equal(toolStatus(row).text, "未执行·平台阻断");
  row.metadata.tool_outcome.invocation_id = "other";
  assert.equal(toolStatus(row).tone, "error");
});

test("归档自报身份不配到当前调用，也不展示为宿主最终判定", () => {
  const use = step(1, "tool_use");
  const material = step(2, "tool_result");
  material.metadata.activity_origin = "archive_material";
  material.metadata.tool_outcome = { ...material.metadata.tool_call_identity, phase: "result", kind: "executed_success", execution_state: "executed" };
  const groups = groupSteps([use, material], false);
  assert.equal(groups.length, 2);
  assert.ok(!groups[0].result);
  assert.equal(toolStatus(material).text, "归档材料·仅供核对");
});
