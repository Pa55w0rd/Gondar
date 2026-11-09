"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import type { DailySchedule } from "@/lib/integration-types";
import { formatIntegrationTime } from "@/lib/integration-view";

export function DailyScheduleCard({ taskId }: { taskId: string }) {
  const fieldId = React.useId();
  const [items, setItems] = React.useState<DailySchedule[]>([]);
  const [clock, setClock] = React.useState("09:00");
  const [timezone, setTimezone] = React.useState("Asia/Shanghai");
  const [runs, setRuns] = React.useState(7);
  const [minutes, setMinutes] = React.useState(60);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [loading, setLoading] = React.useState(false);
  const generation = React.useRef(0);
  const reload = React.useCallback(async () => {
    const id = ++generation.current;
    setLoading(true);
    setError("");
    setItems([]);
    try {
      const rows = await api.dailySchedules(taskId);
      if (id === generation.current) setItems(rows);
    } catch (e) {
      if (id === generation.current) setError(e instanceof Error ? e.message : "读取计划失败");
    } finally {
      if (id === generation.current) setLoading(false);
    }
  }, [taskId]);
  React.useEffect(() => {
    void reload();
    return () => {
      ++generation.current;
    };
  }, [reload]);
  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError("");
    try {
      await fn();
      await reload();
    } catch (e) {
      setError(e instanceof Error ? e.message : "计划操作失败");
    } finally {
      setBusy(false);
    }
  };
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">每日任务计划</CardTitle>
        <CardDescription>
          按当前任务的目标、模型、范围规则与登录配置建立快照，每日创建独立任务。上次尚未结束时跳过；取消只停止未来执行。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <form
          className="flex flex-wrap items-end gap-3"
          onSubmit={(e) => {
            e.preventDefault();
            void act(() =>
              api.createDailySchedule(taskId, {
                daily_at: clock,
                timezone,
                max_runs: runs,
                timeout_seconds: minutes * 60,
              }),
            );
          }}
        >
          <label htmlFor={`${fieldId}-clock`} className="space-y-1 text-xs">
            每日时间
            <Input
              id={`${fieldId}-clock`}
              type="time"
              required
              value={clock}
              onChange={(e) => setClock(e.target.value)}
            />
          </label>
          <label htmlFor={`${fieldId}-timezone`} className="space-y-1 text-xs">
            时区
            <Input
              id={`${fieldId}-timezone`}
              required
              value={timezone}
              onChange={(e) => setTimezone(e.target.value)}
              placeholder="Asia/Shanghai"
            />
          </label>
          <label htmlFor={`${fieldId}-runs`} className="w-28 space-y-1 text-xs">
            最多执行次数
            <Input
              id={`${fieldId}-runs`}
              type="number"
              min={1}
              max={366}
              required
              value={runs}
              onChange={(e) => setRuns(Number(e.target.value))}
            />
          </label>
          <label htmlFor={`${fieldId}-minutes`} className="w-32 space-y-1 text-xs">
            每次预算（分钟）
            <Input
              id={`${fieldId}-minutes`}
              type="number"
              min={1}
              max={1440}
              required
              value={minutes}
              onChange={(e) => setMinutes(Number(e.target.value))}
            />
          </label>
          <Button type="submit" disabled={busy}>
            创建每日计划
          </Button>
        </form>
        <p className="text-xs text-muted-foreground">
          系列最多 {runs} 次，任务运行时间预算合计不超过 {runs * minutes}{" "}
          分钟。夏令时不存在的钟点跳过，重复钟点只执行一次；重启后不批量补跑历史。
        </p>
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        {loading && (
          <p role="status" className="text-sm text-muted-foreground">
            正在读取计划…
          </p>
        )}
        {items.length > 0 && (
          <div className="divide-y rounded border">
            {items.map((plan) => (
              <div key={plan.id} className="flex flex-wrap items-center justify-between gap-2 p-3 text-sm">
                <div className="space-y-1">
                  <p>
                    {plan.daily_at} · {plan.timezone} · {plan.enabled ? "已启用" : "已停止"} · 已创建{" "}
                    {plan.runs_created}/{plan.max_runs} 次 · 跳过 {plan.skipped} 次
                  </p>
                  {plan.enabled && (
                    <p className="text-xs text-muted-foreground">
                      下次：{formatIntegrationTime(plan.next_run_at, plan.timezone)}
                    </p>
                  )}
                  {plan.last_task_id && (
                    <a className="text-xs underline" href={`/function/tasks/detail?id=${plan.last_task_id}`}>
                      最近任务 #{plan.last_task_id}
                    </a>
                  )}
                  {plan.last_error && <p className="text-xs text-amber-700 dark:text-amber-400">{plan.last_error}</p>}
                </div>
                {plan.enabled && (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy}
                    onClick={() => void act(() => api.cancelDailySchedule(taskId, plan.id))}
                  >
                    取消未来执行
                  </Button>
                )}
              </div>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
