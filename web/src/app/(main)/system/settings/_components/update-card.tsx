"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";
import { abortableUpdateDelay, type ExpectedUpdate, UpdateHTTPError, waitForUpdate } from "@/lib/update-confirmation";

/** 等待新版本上线的最长时间。一次升级要经过三次进程启动（暂存 → 换装 → 新版），
 *  每次都是秒级，三分钟足够覆盖慢磁盘和 Docker 容器重建。 */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // SSE仅用于展示，独立确认跨进程核对本次操作和实际运行身份。
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  const [pending, setPending] = React.useState<ExpectedUpdate | null>(null);
  const [confirmationNote, setConfirmationNote] = React.useState("");
  const mounted = React.useRef(false);
  const observer = React.useRef<AbortController | null>(null);
  const stream = React.useRef<EventSource | null>(null);
  const pendingRef = React.useRef<ExpectedUpdate | null>(null);

  const remember = React.useCallback((value: ExpectedUpdate | null) => {
    pendingRef.current = value;
    if (mounted.current) setPending(value);
    try {
      if (value) sessionStorage.setItem("gondar_update_confirmation", JSON.stringify(value));
      else sessionStorage.removeItem("gondar_update_confirmation");
    } catch {
      /* observation remains valid when storage is unavailable */
    }
  }, []);

  const current = React.useCallback(
    (controller: AbortController) => mounted.current && observer.current === controller && !controller.signal.aborted,
    [],
  );
  const beginObserver = React.useCallback(() => {
    observer.current?.abort();
    stream.current?.close();
    const controller = new AbortController();
    observer.current = controller;
    return controller;
  }, []);

  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        if (!mounted.current) return;
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error(`检查更新失败：${r.error}`);
          else if (r.has_update) toast.success(`发现新版本 ${r.latest}`);
          else if (r.comparable) toast.success("当前已是最新版本");
        }
      })
      .catch((e) => {
        if (mounted.current && !quiet) toast.error(`检查更新失败：${(e as Error).message}`);
      })
      .finally(() => {
        if (mounted.current) setChecking(false);
      });
  }, []);

  // Progress frames never decide success. They may be dropped during restart.
  const openStream = React.useCallback(
    (expected: ExpectedUpdate, controller: AbortController) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      stream.current = es;
      es.onmessage = (event) => {
        if (!current(controller)) return;
        let value: UpdateProgress;
        try {
          value = JSON.parse(event.data) as UpdateProgress;
        } catch {
          return;
        }
        if (value.request_id !== expected.request_id) return;
        if (pendingRef.current?.operation_id && value.operation_id !== pendingRef.current.operation_id) return;
        setProgress(value);
        if (value.phase === "staged") setRestarting(true);
      };
      es.onerror = () => es.close();
    },
    [current],
  );

  const observe = React.useCallback(
    async (expected: ExpectedUpdate, existing?: AbortController) => {
      const controller = existing ?? beginObserver();
      if (!existing) openStream(expected, controller);
      remember(expected);
      setBusy(true);
      setConfirmationNote("");
      const result = await waitForUpdate(expected, controller.signal, api.updateStatus, {
        timeoutMs: RESTART_TIMEOUT_MS,
        legacyVersion: api.updateRuntimeVersion,
        onStatus: (status) => {
          if (!current(controller)) return;
          if (status.progress?.operation_id === status.operation.operation_id)
            setProgress(status.progress as UpdateProgress);
          if (["staged", "activating", "settling"].includes(status.operation.phase)) setRestarting(true);
        },
      });
      if (!current(controller)) return;
      stream.current?.close();
      setBusy(false);
      setRestarting(false);
      if (result.kind === "confirmed") {
        setBusy(true);
        remember(null);
        toast.success(`已确认更新到 ${result.version}，正在重新加载页面`);
        await abortableUpdateDelay(800, controller.signal);
        if (current(controller)) window.location.reload();
      } else if (result.kind === "failed" || result.kind === "rolled_back") {
        remember(null);
        setConfirmationNote(result.message);
        toast.error(
          result.kind === "rolled_back" ? `更新已自动回滚：${result.message}` : `更新失败：${result.message}`,
        );
      } else if (result.kind === "auth") {
        setConfirmationNote("登录已过期，请重新登录后继续确认本次更新；后台安装不会因此取消。");
        toast.error("需要重新登录才能确认更新结果");
      } else if (result.kind === "legacy") {
        remember(null);
        setConfirmationNote(
          "目标版本已出现，但服务未提供本次操作的完整身份回执。请手动刷新并核对版本，当前未标记为已确认成功。",
        );
        check(true);
      } else if (result.kind === "timeout") {
        setConfirmationNote("尚未确认本次更新。后台可能仍在下载或重启，可继续确认；不会重复发起安装。");
      }
    },
    [beginObserver, check, current, openStream, remember],
  );

  React.useEffect(() => {
    mounted.current = true;
    check(true);
    try {
      const value = JSON.parse(sessionStorage.getItem("gondar_update_confirmation") || "null") as ExpectedUpdate | null;
      if (
        value &&
        typeof value.request_id === "string" &&
        typeof value.target_version === "string" &&
        typeof value.repository === "string"
      )
        void observe(value);
    } catch {
      /* no saved observation */
    }
    return () => {
      mounted.current = false;
      observer.current?.abort();
      stream.current?.close();
    };
  }, [check, observe]);

  const doUpdate = async () => {
    if (pendingRef.current) {
      void observe(pendingRef.current);
      return;
    }
    if (!info?.latest) return;
    if (
      !window.confirm(
        `确定更新到 ${info.latest}？\n\n更新会重启程序，正在运行的任务会被中断。` +
          (info.mode === "docker" ? "\n容器内更新保留原工具层；重建镜像请使用 docker compose pull。" : ""),
      )
    )
      return;
    const requestID =
      typeof crypto.randomUUID === "function"
        ? crypto.randomUUID()
        : Array.from(crypto.getRandomValues(new Uint8Array(16)), (b) => b.toString(16).padStart(2, "0")).join("");
    const expected: ExpectedUpdate = { request_id: requestID, target_version: info.latest, repository: info.repo };
    const controller = beginObserver();
    remember(expected);
    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "正在提交更新…" });
    openStream(expected, controller);
    try {
      const accepted = await api.applyUpdate({ request_id: requestID, target_version: info.latest }, controller.signal);
      if (!current(controller)) return;
      if (accepted.operation_id) expected.operation_id = accepted.operation_id;
      void observe({ ...expected }, controller);
    } catch (error) {
      if (!current(controller)) return;
      if (error instanceof UpdateHTTPError && error.status >= 400 && error.status < 500) {
        stream.current?.close();
        remember(null);
        setBusy(false);
        setProgress(null);
        toast.error(`启动更新失败：${error.message}`);
      } else {
        // The acknowledgement may be lost after acceptance. Reconcile by request
        // ID instead of issuing another POST or waiting for a staged SSE frame.
        void observe(expected, controller);
      }
    }
  };

  const doRollback = async () => {
    if (!info || !window.confirm("回滚到上一版本？\n\n程序会重启；数据库结构不会回退。")) return;
    const from = info.current;
    const controller = beginObserver();
    setBusy(true);
    try {
      await api.rollbackUpdate();
      if (!current(controller)) return;
      remember(null);
      setRestarting(true);
      const deadline = Date.now() + RESTART_TIMEOUT_MS;
      while (current(controller) && Date.now() < deadline) {
        await abortableUpdateDelay(2000, controller.signal);
        try {
          const version = await api.updateRuntimeVersion(controller.signal);
          if (current(controller) && version.version !== from) {
            toast.success(`回滚后版本为 ${version.version}，正在重新加载`);
            window.location.reload();
            return;
          }
        } catch (error) {
          if (error instanceof UpdateHTTPError && error.status === 401) throw new Error("请重新登录后核对回滚结果");
        }
      }
      if (current(controller)) toast.error("尚未确认回滚后的版本，请检查后端日志");
    } catch (error) {
      if (current(controller)) toast.error(`回滚未确认：${(error as Error).message}`);
    } finally {
      if (current(controller)) {
        setBusy(false);
        setRestarting(false);
      }
    }
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 只有下载阶段拿得到真实百分比（按 Content-Length 算）。校验/解压/等待重启都是
  // 时长不可知的阶段，进度条填满并加个脉冲动画表示"在忙但说不准还要多久"。
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // 设置页是多列瀑布流布局，卡片自己负责行间距并禁止跨列断开（见 page.tsx 的注释）。
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          版本与更新
        </CardTitle>
        <CardDescription>从 GitHub 检查并安装新版本。更新会重启程序，正在运行的任务会被中断。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {confirmationNote && (
          <p role="status" className="text-amber-700 text-sm dark:text-amber-400">
            {confirmationNote}
          </p>
        )}
        {pending && !busy && !restarting && (
          <Button variant="outline" size="sm" onClick={() => void observe(pending)}>
            继续确认本次更新
          </Button>
        )}
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">当前版本</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "独立程序"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">最新版本</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-muted-foreground text-xs underline-offset-4 hover:underline"
            >
              更新日志 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-amber-700 text-xs dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-destructive text-xs">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            <span>
              {info.verification?.ready === false ? "当前构建未启用在线更新：" : "检查发行版本失败："}
              {info.error}
              {info.verification?.ready === false && (
                <> 请使用已配置发布源和发行公钥的正式发行包或 Docker 镜像后重试。</>
              )}
            </span>
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-muted-foreground text-xs">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-destructive text-xs">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest} 没有提供 {info.os}/{info.arch} 的发布包（缺少 {info.asset}），无法自动更新。
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-muted-foreground text-xs">
            将下载 <span className="font-mono">{info.asset}</span>
            {info.size ? `（${humanSize(info.size)}）` : ""}，校验 SHA256 并冒烟测试后才会替换，失败自动保留当前版本。
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-muted-foreground text-xs">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            当前已是最新版本。
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-muted-foreground text-xs">
            Docker 下的更新只替换程序本身，不更新镜像里的 playwright / nmap 等工具链，且
            <span className="font-mono"> docker compose up -d </span>
            重建容器后会退回镜像自带的版本。需要连镜像一起升级请执行
            <span className="font-mono"> docker compose pull gondar &amp;&amp; docker compose up -d gondar</span>。
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-muted-foreground text-xs">
              {restarting ? "正在重启并应用新版本，请稍候（页面会自动刷新）…" : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            检查更新
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `更新到 ${info.latest}` : "立即更新"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              回滚到上一版本
            </Button>
          )}
        </div>

        <p className="text-muted-foreground text-xs">
          一键更新依赖守护脚本重启程序。请通过 <span className="font-mono">start.sh</span>（Windows 为
          <span className="font-mono"> start.bat</span>）启动 Gondar；直接运行 gondar 本体时，程序退出后不会被自动拉起。
        </p>
      </CardContent>
    </Card>
  );
}
