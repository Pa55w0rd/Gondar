"use client";

import * as React from "react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import type { Task } from "@/lib/types";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";

export function TaskExecutionCard({ task, onChange }: { task: Task | null; onChange: (task: Task) => void }) {
  const [mode, setMode] = React.useState<"objective" | "continuous">("objective");
  const [minutes, setMinutes] = React.useState("0");
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => {
    setMode(task?.completion_mode ?? "objective");
    setMinutes(String((task?.timeout_seconds ?? 0) / 60));
  }, [task?.id, task?.completion_mode, task?.timeout_seconds]);
  if (!task) return null;
  const editable = ["paused", "done", "failed", "timeout"].includes(task.status);
  const canContinue = editable && task.completion_mode === "continuous";
  const changed = mode !== (task.completion_mode ?? "objective") || Number(minutes || 0) * 60 !== (task.timeout_seconds ?? 0);
  async function save() {
    if (!task) return;
    const seconds = Number(minutes || 0) * 60;
    if (!Number.isSafeInteger(seconds) || seconds < 0 || seconds > 2147483647) {
      toast.error("请填写有效的非负时限，0或留空表示不限时");
      return;
    }
    setBusy(true);
    try {
      const patch: { completion_mode?: "objective" | "continuous"; timeout_seconds?: number } = {};
      if (mode !== (task.completion_mode ?? "objective")) patch.completion_mode = mode;
      if (seconds !== (task.timeout_seconds ?? 0)) patch.timeout_seconds = seconds;
      onChange(await api.updateTaskExecution(task.id, patch));
      toast.success("设置已保存，任务尚未启动");
    } catch (error) { toast.error((error as Error).message); }
    finally { setBusy(false); }
  }
  async function continueExploration() {
    if (!task) return;
    setBusy(true);
    try {
      const result = await api.continueTask(task.id);
      onChange(await api.task(task.id));
      toast.success(result.queued ? "已保留成果并加入继续队列" : "已保留成果并继续挖掘");
    } catch (error) { toast.error((error as Error).message); }
    finally { setBusy(false); }
  }
  return (
    <Card>
      <CardHeader>
        <CardTitle>完成策略与任务时限</CardTitle>
        <CardDescription>持续挖掘中的单项目标完成不会结束整个任务；无进展、预算和连续故障保护仍有效。已有成果会保留。</CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-4">
        <label className="grid gap-2 text-sm">
          完成策略
          <NativeSelect value={mode} disabled={!editable || busy} onChange={(e) => setMode(e.target.value as "objective" | "continuous")}>
            <NativeSelectOption value="continuous">持续挖掘</NativeSelectOption>
            <NativeSelectOption value="objective">目标达成后结束</NativeSelectOption>
          </NativeSelect>
        </label>
        <label className="grid gap-2 text-sm">
          任务时限（分钟，0或留空表示不限时）
          <Input className="max-w-48" type="number" min={0} step="any" value={minutes} disabled={!editable || busy} onChange={(e) => setMinutes(e.target.value)} />
        </label>
        <p className="text-muted-foreground text-xs">请先暂停并等待执行停止后修改。修改时限后，下次继续会重新计时。保存设置不会自动发起测试；继续不会批量重跑旧意图。</p>
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" disabled={!editable || busy || !changed} onClick={() => void save()}>保存设置</Button>
          {canContinue && <Button disabled={busy || changed} onClick={() => void continueExploration()}>保留成果并继续挖掘</Button>}
        </div>
      </CardContent>
    </Card>
  );
}
