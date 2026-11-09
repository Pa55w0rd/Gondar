"use client";

import * as React from "react";

import Link from "next/link";

import { Loader2, RefreshCw, Send, Square } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { RepeaterPolicy, RepeaterRun } from "@/lib/repeater-types";
import { repeaterBodyView, repeaterLineChanges, repeaterRequestWithHost } from "@/lib/repeater-view";
import type { Task } from "@/lib/types";

const STATE_LABEL: Record<RepeaterRun["state"], string> = {
  running: "处理中",
  completed: "已响应",
  blocked: "被阻止",
  canceled: "已取消",
  error: "发送失败",
};

export default function RepeaterPage() {
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const [taskId, setTaskId] = React.useState("");
  const [policy, setPolicy] = React.useState<RepeaterPolicy | null>(null);
  const [policyError, setPolicyError] = React.useState("");
  const [origin, setOrigin] = React.useState("");
  const [raw, setRaw] = React.useState("GET / HTTP/1.1\r\nHost: \r\n\r\n");
  const [sourceId, setSourceId] = React.useState("");
  const [timeout, setTimeoutSeconds] = React.useState(20);
  const [runs, setRuns] = React.useState<RepeaterRun[]>([]);
  const [result, setResult] = React.useState<RepeaterRun | null>(null);
  const [baseline, setBaseline] = React.useState<RepeaterRun | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [loadingHistory, setLoadingHistory] = React.useState(false);
  const [cancelMessage, setCancelMessage] = React.useState("");
  const abort = React.useRef<AbortController | null>(null);

  React.useEffect(() => {
    let active = true;
    api
      .tasks()
      .then((r) => {
        if (active) setTasks(r.tasks);
      })
      .catch((e) => toast.error(String(e)));
    const query = new URL(window.location.href).searchParams;
    const source = query.get("source");
    const inputOrigin = query.get("origin");
    if (source && inputOrigin) {
      try {
        const u = new URL(inputOrigin);
        if (!["http:", "https:"].includes(u.protocol) || u.username || u.password) throw new Error("来源目标无效");
        api
          .trafficExchange(source)
          .then((r) => {
            if (!active) return;
            setOrigin(u.origin);
            setRaw(repeaterRequestWithHost(r.req, u.origin));
            setSourceId(source);
          })
          .catch((e) => toast.error(`未能导入流量：${String(e)}`));
      } catch {
        toast.error("来源连接目标无效，请手动填入");
      }
    }
    return () => {
      active = false;
      abort.current?.abort();
    };
  }, []);

  const refresh = React.useCallback(async () => {
    if (!taskId) return;
    const [p, history] = await Promise.all([api.repeaterPolicy(taskId), api.repeaterRuns(taskId)]);
    setPolicy(p);
    setPolicyError("");
    setRuns(history);
  }, [taskId]);

  React.useEffect(() => {
    let active = true;
    setPolicy(null);
    setPolicyError("");
    setRuns([]);
    setResult(null);
    setBaseline(null);
    setCancelMessage("");
    if (!taskId) return;
    Promise.all([api.repeaterPolicy(taskId), api.repeaterRuns(taskId)])
      .then(([p, history]) => {
        if (active) {
          setPolicy(p);
          setRuns(history);
        }
      })
      .catch((e) => {
        if (active) setPolicyError(String(e));
      });
    return () => {
      active = false;
    };
  }, [taskId]);

  // A disconnected browser cannot receive the final canceled result. Refresh
  // boundedly while the server persists cancellation; never repeat the request.
  React.useEffect(() => {
    if (!cancelMessage || !taskId) return;
    let active = true;
    let checks = 0;
    const timer = setInterval(() => {
      if (++checks > 10) {
        clearInterval(timer);
        return;
      }
      api
        .repeaterRuns(taskId)
        .then((history) => {
          if (!active) return;
          setRuns(history);
          if (!history.some((r) => r.state === "running")) clearInterval(timer);
        })
        .catch(() => clearInterval(timer));
    }, 1000);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [cancelMessage, taskId]);

  async function send() {
    if (busy || !taskId) return;
    const controller = new AbortController();
    abort.current = controller;
    setBusy(true);
    setCancelMessage("");
    try {
      const run = await api.repeaterSend(
        {
          task_id: taskId,
          origin,
          raw_request: raw,
          source_traffic_id: sourceId || undefined,
          timeout_seconds: timeout,
        },
        controller.signal,
      );
      setResult(run);
      if (run.state !== "completed") toast.error(run.error || STATE_LABEL[run.state]);
    } catch (e) {
      if (controller.signal.aborted) setCancelMessage("已请求取消。执行结果保存在下方历史中，可刷新核对。");
      else toast.error(String(e));
    } finally {
      abort.current = null;
      await refresh().catch((e) => toast.error(`历史刷新失败：${String(e)}`));
      setBusy(false);
    }
  }

  async function loadRun(id: number, asBaseline: boolean) {
    setLoadingHistory(true);
    try {
      const run = await api.repeaterRun(taskId, id);
      if (asBaseline) setBaseline(run);
      else {
        setResult(run);
        setOrigin(new URL(run.url).origin);
        setRaw(run.request || "");
        setSourceId(run.source_traffic_id || "");
      }
    } catch (e) {
      toast.error(String(e));
    } finally {
      setLoadingHistory(false);
    }
  }

  const bodyView = repeaterBodyView(result?.response_body_base64, result?.response_head);
  const baseView = repeaterBodyView(baseline?.response_body_base64, baseline?.response_head);
  const bodyText = bodyView.text;
  const baseBody = baseView.text;
  const requestBytes = new TextEncoder().encode(raw).length;
  const quotaFull = Boolean(
    policy &&
      (policy.run_count >= policy.run_limit || policy.reserved_bytes + requestBytes + 327680 > policy.storage_limit),
  );
  const ready = Boolean(
    taskId && policy?.capture_enabled && policy.allow_rules.length && origin.trim() && raw.trim() && !quotaFull,
  );
  const responseText = `${result?.response_head || ""}\r\n${bodyText}`;

  return (
    <div className="flex flex-col gap-5 p-4 md:p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="font-semibold text-2xl tracking-tight">HTTP Repeater</h1>
          <p className="mt-1 text-muted-foreground text-sm">编辑请求，复用任务审批与录制链路，逐次核对响应。</p>
        </div>
        <Button
          variant="outline"
          size="sm"
          disabled={!taskId || busy}
          onClick={() => void refresh().catch((e) => toast.error(String(e)))}
        >
          <RefreshCw className="size-4" />
          刷新规则与历史
        </Button>
      </div>

      <div className="grid gap-4 border-b pb-4 lg:grid-cols-[minmax(16rem,1fr)_2fr]">
        <div className="space-y-2">
          <Label htmlFor="repeater-task">所属任务</Label>
          <select
            id="repeater-task"
            className="h-9 w-full rounded-md border bg-background px-3 text-sm"
            value={taskId}
            disabled={busy || loadingHistory}
            onChange={(e) => setTaskId(e.target.value)}
          >
            <option value="">选择任务后发送</option>
            {tasks.map((t) => (
              <option key={t.id} value={t.id}>
                #{t.id} {t.name || t.description.slice(0, 60)}
              </option>
            ))}
          </select>
          {taskId && (
            <Link
              className="text-primary text-xs underline underline-offset-4"
              href={`/function/tasks/detail?id=${encodeURIComponent(taskId)}&tab=overview`}
            >
              打开任务总览中的“任务级资产拦截 / 允许规则”
            </Link>
          )}
        </div>
        <div className="space-y-1 text-sm" role="status">
          {policyError && <p className="text-destructive">{policyError}</p>}
          {!policyError && !taskId && (
            <p className="text-muted-foreground">选择任务以读取允许规则与历史。导入流量不会自动发送。</p>
          )}
          {!policyError && taskId && !policy && <p>正在读取任务规则…</p>}
          {!policyError && taskId && policy && (
            <>
              <p>
                <Badge variant={policy.capture_enabled ? "secondary" : "destructive"}>
                  {policy.capture_enabled ? "流量录制已启用" : "请在系统配置中启用流量录制"}
                </Badge>
              </p>
              {!policy.allow_rules.length ? (
                <p className="text-destructive">
                  当前没有启用的精确允许项。请在任务总览添加允许规则；模糊匹配不作为发送授权。
                </p>
              ) : (
                <p className="break-all text-muted-foreground">
                  允许：{policy.allow_rules.map((r) => `${r.kind}: ${r.pattern}`).join("；")}
                </p>
              )}
              <p className="text-muted-foreground text-xs">
                阻止规则仍然优先。连接目标与 Host 必须一致；重定向只显示响应。请求最多 128 KiB，正文 64 KiB；响应保留前
                256 KiB。
              </p>
              <p className="text-muted-foreground text-xs">
                历史保留：{policy.run_count}/{policy.run_limit} 次；报文字节（含进行中预留）
                {(policy.reserved_bytes / 1048576).toFixed(2)}/{policy.storage_limit / 1048576} MiB。
              </p>
              {quotaFull && (
                <p className="text-destructive text-sm">本任务历史保留配额不足，请在新任务继续。已有证据会完整保留。</p>
              )}
            </>
          )}
        </div>
      </div>

      <div className="grid min-w-0 gap-5 xl:grid-cols-2">
        <section className="min-w-0 space-y-3" aria-label="请求编辑">
          <div className="flex items-center justify-between gap-2">
            <h2 className="font-medium">请求</h2>
            <span className="text-muted-foreground text-xs tabular-nums">
              {requestBytes.toLocaleString()} 字节{sourceId ? " · 来自录制流量" : ""}
            </span>
          </div>
          <div className="space-y-2">
            <Label htmlFor="repeater-origin">连接目标（协议、主机、端口）</Label>
            <Input
              id="repeater-origin"
              className="font-mono"
              placeholder="https://console.example.test"
              value={origin}
              disabled={busy}
              onChange={(e) => setOrigin(e.target.value)}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="repeater-raw">原始 HTTP/1 请求</Label>
            <Textarea
              id="repeater-raw"
              spellCheck={false}
              className="min-h-96 resize-y whitespace-pre font-mono text-xs leading-5"
              value={raw}
              disabled={busy}
              onChange={(e) => setRaw(e.target.value)}
            />
          </div>
          <div className="flex flex-wrap items-center gap-3">
            <Button disabled={!ready || busy || requestBytes > 131072} onClick={() => void send()}>
              {busy ? <Loader2 className="size-4 animate-spin" /> : <Send className="size-4" />}发送请求
            </Button>
            {busy && (
              <Button variant="outline" onClick={() => abort.current?.abort()}>
                <Square className="size-4" />
                取消发送
              </Button>
            )}
            <Label htmlFor="repeater-timeout" className="text-xs">
              超时（秒）
            </Label>
            <Input
              id="repeater-timeout"
              className="w-20"
              type="number"
              min={1}
              max={60}
              value={timeout}
              disabled={busy}
              onChange={(e) => setTimeoutSeconds(Math.max(1, Math.min(60, Number(e.target.value) || 20)))}
            />
          </div>
          <p className="text-muted-foreground text-xs">
            按 HTTP 语义发送并重算 Content-Length，不支持
            CONNECT、分块请求或协议升级。任务凭据随报文保存在本任务历史中。
          </p>
          {cancelMessage && (
            <p className="text-sm" role="status">
              {cancelMessage}
            </p>
          )}
        </section>

        <section className="min-w-0 space-y-3" aria-label="响应与对比">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 className="font-medium">响应</h2>
            {result && (
              <div className="flex gap-2">
                <Badge variant={result.state === "completed" ? "secondary" : "destructive"}>
                  {STATE_LABEL[result.state]}
                </Badge>
                <span className="text-sm tabular-nums">
                  {result.status || "—"} / {result.elapsed_ms} ms / {result.response_bytes.toLocaleString()} B
                </span>
              </div>
            )}
          </div>
          {result?.error && <p className="break-words text-destructive text-sm">{result.error}</p>}
          {result?.truncated && <p className="text-amber-600 text-sm">响应已截断；摘要与对比只覆盖已保存部分。</p>}
          {result?.traffic_id && (
            <p className="break-all text-muted-foreground text-xs">证据流量 ID：{result.traffic_id}</p>
          )}
          {result?.capture_note && <p className="text-amber-600 text-xs">{result.capture_note}</p>}
          <pre
            className="max-h-[38rem] min-h-96 overflow-auto rounded-md border bg-muted/30 p-3 font-mono text-xs leading-5"
            aria-live="polite"
          >
            {result ? responseText : "发送请求或载入历史后，在这里查看响应。返回的 HTML 仅作为文本显示。"}
          </pre>
          {result?.response_sha256 && (
            <p className="break-all text-muted-foreground text-xs">正文 SHA-256：{result.response_sha256}</p>
          )}
          <div className="flex items-center gap-2">
            <Label htmlFor="repeater-baseline" className="shrink-0">
              对比基线
            </Label>
            <select
              id="repeater-baseline"
              className="h-9 min-w-0 flex-1 rounded-md border bg-background px-2 text-sm"
              value={baseline?.id || ""}
              disabled={loadingHistory || busy}
              onChange={(e) => (e.target.value ? void loadRun(Number(e.target.value), true) : setBaseline(null))}
            >
              <option value="">选择本任务历史</option>
              {runs.map((r) => (
                <option key={r.id} value={r.id}>
                  #{r.id} {r.method} {r.status || STATE_LABEL[r.state]} {r.url.slice(0, 80)}
                </option>
              ))}
            </select>
          </div>
          {result && baseline && (
            <div className="space-y-2 border-t pt-3">
              <p className="text-sm">
                基线 #{baseline.id} → 当前 #{result.id}：状态 {baseline.status} → {result.status}，正文{" "}
                {baseline.response_bytes} → {result.response_bytes} 字节，摘要
                {baseline.response_sha256 === result.response_sha256 ? "相同" : "不同"}。
              </p>
              {(baseline.truncated || result.truncated) && (
                <p className="text-amber-600 text-xs">至少一份响应被截断，不能据此判定完整响应相同。</p>
              )}
              <pre className="max-h-72 overflow-auto rounded-md border p-3 font-mono text-xs leading-5">
                {bodyView.comparable && baseView.comparable
                  ? repeaterLineChanges(`${baseline.response_head || ""}\r\n${baseBody}`, responseText)
                  : `二进制或压缩正文按字节数与 SHA-256 对比。以下仅比较响应头：\n${repeaterLineChanges(baseline.response_head || "", result.response_head || "")}`}
              </pre>
            </div>
          )}
        </section>
      </div>

      <section className="min-w-0 border-t pt-4" aria-label="执行历史">
        <h2 className="mb-3 font-medium">本任务最近 50 次执行</h2>
        {!runs.length ? (
          <p className="text-muted-foreground text-sm">选择任务后，每次发送、阻止或取消都会保存在这里。</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="text-muted-foreground">
                <tr className="border-b">
                  <th className="p-2">记录 / 时间</th>
                  <th className="p-2">请求</th>
                  <th className="p-2">结果</th>
                  <th className="p-2">响应字节 / 耗时</th>
                  <th className="p-2">操作</th>
                </tr>
              </thead>
              <tbody>
                {runs.map((r) => (
                  <tr key={r.id} className="border-b">
                    <td className="whitespace-nowrap p-2">
                      #{r.id}
                      <span className="ml-2 text-muted-foreground text-xs">
                        {new Date(r.created_at).toLocaleString("zh-CN")}
                      </span>
                    </td>
                    <td className="max-w-96 truncate p-2 font-mono text-xs" title={r.url}>
                      {r.method} {r.url}
                    </td>
                    <td className="p-2">
                      {STATE_LABEL[r.state]} {r.status || ""}
                      {r.truncated ? "（截断）" : ""}
                    </td>
                    <td className="whitespace-nowrap p-2 tabular-nums">
                      {r.response_bytes} B / {r.elapsed_ms} ms
                    </td>
                    <td className="p-2">
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={busy || loadingHistory}
                        onClick={() => void loadRun(r.id, false)}
                      >
                        载入记录
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
