"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import type { TaskLesson } from "@/lib/integration-types";

const labels = { pending: "待复核", accepted: "已接受", rejected: "已拒绝", retired: "已停用" };

function LessonItem({ taskId, lesson, refresh }: { taskId: string; lesson: TaskLesson; refresh: () => Promise<void> }) {
  const daysId = React.useId();
  const [shared, setShared] = React.useState(lesson.shared);
  const [note, setNote] = React.useState(lesson.review_note ?? "");
  const [days, setDays] = React.useState(30);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const review = async (status: "accepted" | "rejected" | "retired") => {
    setBusy(true);
    setError("");
    try {
      await api.reviewTaskLesson(taskId, lesson.id, {
        revision: lesson.revision,
        status,
        shared,
        note,
        valid_days: days,
      });
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : "复核失败");
    } finally {
      setBusy(false);
    }
  };
  return (
    <details className="rounded border p-3">
      <summary className="cursor-pointer text-sm">
        #{lesson.id} · {labels[lesson.status]} · {lesson.tool} · {lesson.summary}
      </summary>
      <div className="mt-3 space-y-3 text-sm">
        <p className="whitespace-pre-wrap break-words">{lesson.resolution}</p>
        <p className="text-xs text-muted-foreground">
          标签：{lesson.tags.join("、") || "无"} · 版本 {lesson.revision} · 提议者 {lesson.origin} · 有效期至{" "}
          {new Date(lesson.expires_at).toLocaleString("zh-CN")}
        </p>
        <div className="space-y-2">
          {lesson.evidence?.map((evidence) => (
            <div key={evidence.activity_id} className="rounded bg-muted p-2 text-xs">
              <p>
                工具活动 #{evidence.activity_id} · {evidence.tool} · {evidence.is_error ? "错误结果" : "正常结果"}
              </p>
              <pre className="whitespace-pre-wrap break-all">{evidence.excerpt}</pre>
              <p className="break-all text-muted-foreground">证据摘要：{evidence.sha256}</p>
            </div>
          ))}
        </div>
        <p className="text-xs text-muted-foreground">
          出处真实只说明可追溯，请独立核验建议是否正确、适用条件是否完整。接受后的经验可供模型按需召回。
        </p>
        <label className="flex items-center gap-2 text-xs">
          <input type="checkbox" checked={shared} onChange={(e) => setShared(e.target.checked)} />
          允许其他任务调用此经验（先检查是否包含目标数据或凭据）
        </label>
        <div className="flex flex-wrap gap-2">
          <Input
            className="min-w-48 flex-1"
            value={note}
            maxLength={1000}
            onChange={(e) => setNote(e.target.value)}
            placeholder="复核说明与适用限制"
            aria-label="经验复核说明"
          />
          <label htmlFor={daysId} className="flex items-center gap-1 text-xs">
            有效天数
            <Input
              id={daysId}
              className="w-20"
              type="number"
              min={1}
              max={180}
              value={days}
              onChange={(e) => setDays(Number(e.target.value))}
            />
          </label>
          <Button
            size="sm"
            disabled={busy || !Number.isInteger(days) || days < 1 || days > 180}
            onClick={() => void review("accepted")}
          >
            接受
          </Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => void review("rejected")}>
            拒绝
          </Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => void review("retired")}>
            停用
          </Button>
        </div>
        {error && (
          <p role="alert" className="text-xs text-destructive">
            {error}
          </p>
        )}
      </div>
    </details>
  );
}

export function TaskLessonsCard({ taskId }: { taskId: string }) {
  const [lessons, setLessons] = React.useState<TaskLesson[]>([]);
  const [error, setError] = React.useState("");
  const [loading, setLoading] = React.useState(false);
  const generation = React.useRef(0);
  const refresh = React.useCallback(async () => {
    const id = ++generation.current;
    setLoading(true);
    setError("");
    setLessons([]);
    try {
      const rows = await api.taskLessons(taskId);
      if (id === generation.current) setLessons(rows);
    } catch (e) {
      if (id === generation.current) setError(e instanceof Error ? e.message : "读取经验失败");
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
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">可复用经验</CardTitle>
        <CardDescription>
          模型通过 propose_lesson
          提议带真实工具出处的经验，接受后才进入召回。经验与本次目标事实分别存储，默认仅本任务使用。
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-2">
        <Button size="sm" variant="outline" onClick={() => void refresh().catch((e) => setError(String(e)))}>
          刷新经验
        </Button>
        {loading && (
          <p role="status" className="text-sm text-muted-foreground">
            正在读取经验…
          </p>
        )}
        {!loading && !error && lessons.length === 0 && (
          <p className="text-sm text-muted-foreground">
            暂无经验提议。可在任务对话中要求整理成功方法或失败教训，并引用真实工具活动。
          </p>
        )}
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        {lessons.map((lesson) => (
          <LessonItem key={`${lesson.id}-${lesson.revision}`} taskId={taskId} lesson={lesson} refresh={refresh} />
        ))}
      </CardContent>
    </Card>
  );
}
