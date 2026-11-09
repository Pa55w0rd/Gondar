"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import type { TaskResource } from "@/lib/integration-types";

const stateLabels = {
  planned: "待创建",
  created: "已创建",
  active: "活跃",
  cleanup_pending: "待清理",
  cleaned: "已清理",
  failed: "失败或残留待处理",
};
const kindLabels = {
  local_file: "本地文件",
  upload: "上传文件",
  account: "临时账号",
  session: "会话",
  listener: "监听器",
};

export function TaskResourcesCard({ taskId }: { taskId: string }) {
  const [items, setItems] = React.useState<TaskResource[]>([]);
  const [error, setError] = React.useState("");
  const [loading, setLoading] = React.useState(false);
  const generation = React.useRef(0);
  const refresh = React.useCallback(async () => {
    const id = ++generation.current;
    setLoading(true);
    setError("");
    setItems([]);
    try {
      const rows = await api.taskResources(taskId);
      if (id === generation.current) setItems(rows);
    } catch (e) {
      if (id === generation.current) setError(e instanceof Error ? e.message : "读取台账失败");
    } finally {
      if (id === generation.current) setLoading(false);
    }
  }, [taskId]);
  React.useEffect(() => {
    void refresh();
    return () => {
      ++generation.current;
    };
  }, [refresh]);
  const remaining = items.filter((item) => item.state !== "cleaned").length;
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">资源与清理台账</CardTitle>
        <CardDescription>
          跟踪临时账号、上传、会话与文件的归属和残留。登记不会执行创建或清理，也不能代替目标成功证明。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex items-center justify-between gap-2">
          <p className="text-sm">
            {loading || error ? "台账尚未就绪" : `共 ${items.length} 项，尚未清理 ${remaining} 项`}
          </p>
          <Button size="sm" variant="outline" onClick={() => void refresh().catch((e) => setError(String(e)))}>
            刷新台账
          </Button>
        </div>
        {loading && (
          <p role="status" className="text-sm text-muted-foreground">
            正在读取台账…
          </p>
        )}
        {!loading && !error && items.length === 0 && (
          <p className="text-sm text-muted-foreground">
            暂无资源记录。任务执行时可通过对话要求登记资源、清理计划与实际结果。
          </p>
        )}
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        {items.map((item) => (
          <details key={`${item.id}-${item.revision}`} className="rounded border p-3 text-sm">
            <summary className="cursor-pointer break-all">
              #{item.id} · {stateLabels[item.state]} · {kindLabels[item.kind]} · {item.name}
            </summary>
            <div className="mt-3 space-y-2">
              <p>
                关联资产：{item.asset_id ? `#${item.asset_id}` : "无"} · 版本 {item.revision} · 登记者 {item.origin}
              </p>
              <p className="whitespace-pre-wrap break-all">清理计划：{item.cleanup}</p>
              <p className="whitespace-pre-wrap break-all">实际结果：{item.note || "尚未记录"}</p>
              <p className="text-xs text-muted-foreground">
                工具结果依据：{item.evidence_ids.length ? item.evidence_ids.map((id) => `#${id}`).join("、") : "尚无"}
              </p>
              <p
                className={`text-xs ${item.state !== "cleaned" && new Date(item.expires_at).getTime() < Date.now() ? "text-amber-700 dark:text-amber-400" : "text-muted-foreground"}`}
              >
                复核期限：{new Date(item.expires_at).toLocaleString("zh-CN")}；到期后需重新确认状态。
              </p>
            </div>
          </details>
        ))}
      </CardContent>
    </Card>
  );
}
