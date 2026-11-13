import type { Activity } from "./types";

export type TranscriptGroup =
  | { type: "user"; key: number; step: Activity; intent?: boolean }
  | { type: "answer"; key: number; step: Activity }
  | { type: "round"; key: number; label: string }
  | { type: "tool"; key: number; worker: string; use?: Activity; result?: Activity }
  | { type: "msg"; key: number; worker: string; steps: Activity[] }
  | { type: "intercept"; key: number; step: Activity };

function exactKey(step: Activity): string | undefined {
  const id = step.metadata?.tool_call_identity;
  if (
    id?.version !== 1 ||
    !id.run_id ||
    !id.prompt_id ||
    !id.invocation_id ||
    !id.tool_use_id ||
    id.tool_use_id !== step.tool_use_id
  )
    return;
  return JSON.stringify([
    step.metadata?.activity_origin ?? "legacy",
    step.source_task_id ?? "",
    step.worker,
    step.intent_id ?? "",
    id.run_id,
    id.prompt_id,
    id.invocation_id,
  ]);
}

function legacyKey(step: Activity): string | undefined {
  if (!step.tool_use_id) return;
  return `${step.source_task_id ?? ""}:${step.worker}:${step.intent_id ?? ""}:${step.main_seg ?? 0}:${step.tool_use_id}`;
}

export function groupSteps(steps: Activity[], chat: boolean): TranscriptGroup[] {
  const out: TranscriptGroup[] = [];
  const exact = new Map<string, Extract<TranscriptGroup, { type: "tool" }>>();
  const ambiguous = new Set<string>();
  const legacy = new Map<string, Extract<TranscriptGroup, { type: "tool" }>[]>();
  for (const step of steps) {
    if (step.kind === "usage") continue;
    if (step.kind === "round") {
      legacy.clear();
      out.push({ type: "round", key: step.seq, label: step.summary || "新一轮" });
    } else if (step.kind === "intercept_request") {
      out.push({ type: "intercept", key: step.seq, step });
    } else if (step.kind === "user" || step.kind === "intent") {
      out.push({ type: "user", key: step.seq, step, intent: step.kind === "intent" });
    } else if (step.kind === "result" || (chat && step.kind === "text")) {
      out.push({ type: "answer", key: step.seq, step });
    } else if (step.kind === "tool_use") {
      const group: Extract<TranscriptGroup, { type: "tool" }> = {
        type: "tool",
        key: step.seq,
        worker: step.worker,
        use: step,
      };
      const key = exactKey(step);
      if (key) {
        if (exact.has(key) || ambiguous.has(key)) {
          exact.delete(key);
          ambiguous.add(key);
        } else exact.set(key, group);
      } else {
        const oldKey = legacyKey(step);
        if (oldKey) legacy.set(oldKey, [...(legacy.get(oldKey) ?? []), group]);
      }
      out.push(group);
    } else if (step.kind === "tool_result" || step.kind === "tool_late_result") {
      const key = exactKey(step);
      const candidates = key ? [exact.get(key)] : (legacy.get(legacyKey(step) ?? "") ?? []).filter((g) => !g.result);
      const group = candidates.length === 1 ? candidates[0] : undefined;
      if (step.kind !== "tool_late_result" && group && !group.result) group.result = step;
      else out.push({ type: "tool", key: step.seq, worker: step.worker, result: step });
    } else {
      const last = out[out.length - 1];
      if (last?.type === "msg" && last.worker === step.worker) last.steps.push(step);
      else out.push({ type: "msg", key: step.seq, worker: step.worker, steps: [step] });
    }
  }
  return out;
}

export function toolStatus(result?: Activity): { text: string; tone: "running" | "ok" | "blocked" | "error" } {
  if (!result) return { text: "执行中…", tone: "running" };
  if (result.metadata?.activity_origin === "archive_material") return { text: "归档材料·仅供核对", tone: "blocked" };
  const out = result.metadata?.tool_outcome;
  const id = result.metadata?.tool_call_identity;
  if (
    result.metadata?.activity_origin === "runtime" &&
    out?.version === 1 &&
    exactKey(result) &&
    out.run_id === id?.run_id &&
    out.prompt_id === id.prompt_id &&
    out.invocation_id === id.invocation_id &&
    out.tool_use_id === id.tool_use_id
  ) {
    if (out.phase === "late_receipt") return { text: "补收执行回执", tone: "blocked" };
    if (["policy_denied", "permission_denied", "approval_rejected", "approval_timeout"].includes(out.kind))
      return { text: out.kind === "approval_timeout" ? "未执行·审批超时" : "未执行·平台阻断", tone: "blocked" };
    if (["schema_invalid", "unknown_tool"].includes(out.kind)) return { text: "参数/工具待修正", tone: "blocked" };
    if (out.kind === "scheduler_deferred") return { text: "等待执行容量", tone: "blocked" };
    if (["precondition_missing", "evidence_incomplete"].includes(out.kind))
      return { text: "前提/证据待补齐", tone: "blocked" };
    if (["cancelled", "configured_deadline"].includes(out.kind) || out.execution_state === "unknown")
      return { text: out.execution_state === "not_executed" ? "已中断·未执行" : "已中断·结果未知", tone: "blocked" };
    if (out.kind === "hook_prevented") return { text: "未执行·宿主阻止", tone: "blocked" };
  }
  return result.is_error ? { text: "✕ 失败", tone: "error" } : { text: "✓", tone: "ok" };
}
