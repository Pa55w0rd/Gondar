"use client";

import * as React from "react";

import { ArrowUpIcon, DownloadIcon, FileTextIcon, FolderIcon, RefreshCwIcon, XIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { TaskFileEntry, TaskFileListing, WorkspaceFile } from "@/lib/types";

function fileSize(bytes: number) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 ** 2) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / 1024 ** 2).toFixed(1)} MiB`;
}

export function FilesTab({ taskId }: { taskId: string }) {
  const [path, setPath] = React.useState("");
  const [input, setInput] = React.useState("");
  const [mode, setMode] = React.useState("browse");
  const [refresh, setRefresh] = React.useState(0);
  const [listing, setListing] = React.useState<TaskFileListing | null>(null);
  const [error, setError] = React.useState("");
  const [loading, setLoading] = React.useState(false);
  const [selected, setSelected] = React.useState<TaskFileEntry | null>(null);
  const [preview, setPreview] = React.useState<WorkspaceFile | null>(null);
  const [previewError, setPreviewError] = React.useState("");
  const [previewLoading, setPreviewLoading] = React.useState(false);
  const [downloading, setDownloading] = React.useState("");
  const downloadController = React.useRef<AbortController | null>(null);

  React.useEffect(() => {
    void refresh; // manual refresh intentionally starts a fresh bounded request
    const controller = new AbortController();
    setLoading(true);
    setError("");
    setListing(null);
    api
      .taskFiles(taskId, path, mode === "recent", controller.signal)
      .then((result) => {
        if (!controller.signal.aborted) setListing(result);
      })
      .catch((err: Error) => {
        if (!controller.signal.aborted) setError(err.message);
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [mode, path, refresh, taskId]);

  React.useEffect(() => {
    setPreview(null);
    setPreviewError("");
    setPreviewLoading(false);
    if (!selected) return;
    const controller = new AbortController();
    setPreviewLoading(true);
    api
      .taskFileRead(taskId, selected.path, controller.signal)
      .then((result) => {
        if (!controller.signal.aborted) setPreview(result);
      })
      .catch((err: Error) => {
        if (!controller.signal.aborted) setPreviewError(err.message);
      })
      .finally(() => {
        if (!controller.signal.aborted) setPreviewLoading(false);
      });
    return () => controller.abort();
  }, [selected, taskId]);

  React.useEffect(
    () => () => {
      downloadController.current?.abort();
    },
    [],
  );

  function navigate(next: string) {
    setPath(next);
    setInput(next);
    setSelected(null);
  }
  async function download(file: string, zip = false) {
    if (downloadController.current) return;
    const controller = new AbortController();
    downloadController.current = controller;
    setDownloading(zip ? "zip" : file);
    try {
      await api.taskFileDownload(taskId, file, zip, controller.signal);
      if (!controller.signal.aborted) toast.success("已开始保存下载文件");
    } catch (err) {
      if (!controller.signal.aborted) toast.error((err as Error).message);
    } finally {
      if (downloadController.current === controller) {
        downloadController.current = null;
        if (!controller.signal.aborted) setDownloading("");
      }
    }
  }
  function cancelDownload() {
    downloadController.current?.abort();
    downloadController.current = null;
    setDownloading("");
  }
  const parent = path.split("/").filter(Boolean).slice(0, -1).join("/");
  const incomplete = listing?.truncated || listing?.scan?.truncated;
  let previewContent: React.ReactNode;
  if (previewLoading)
    previewContent = (
      <div className="flex items-center gap-2 p-5">
        <Spinner />
        加载中…
      </div>
    );
  else if (previewError)
    previewContent = (
      <p role="alert" className="p-5 text-destructive">
        {previewError}
      </p>
    );
  else if (preview?.binary)
    previewContent = (
      <p className="p-5 text-muted-foreground">
        {preview.too_large ? "文件超过预览上限，请下载后查看。" : "该文件不是可预览的 UTF-8 文本，请下载后查看。"}
      </p>
    );
  else
    previewContent = (
      <pre className="min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-all p-5 font-mono text-xs">
        {preview?.content}
      </pre>
    );

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h2 className="font-semibold">任务文件</h2>
          <p className="text-muted-foreground text-xs">
            只读查看当前任务生成的文件；关联任务的目录不在此入口开放。最近文件按需刷新。
          </p>
        </div>
        <Tabs value={mode} onValueChange={setMode}>
          <TabsList>
            <TabsTrigger value="browse">目录浏览</TabsTrigger>
            <TabsTrigger value="recent">最近文件</TabsTrigger>
          </TabsList>
        </Tabs>
      </div>
      <form
        className="flex flex-wrap items-center gap-2"
        onSubmit={(event) => {
          event.preventDefault();
          navigate(input.replaceAll("\\", "/"));
        }}
      >
        <Button type="button" size="sm" variant="outline" onClick={() => navigate("")} disabled={!path}>
          任务根目录
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => navigate(parent)}
          disabled={!path}
          aria-label="上一级目录"
        >
          <ArrowUpIcon className="size-4" />
        </Button>
        <Input
          aria-label="任务内目录路径"
          value={input}
          onChange={(event) => setInput(event.target.value)}
          placeholder="任务内相对目录，例如 intent-42"
          className="min-w-40 flex-1 font-mono text-xs"
        />
        <Button size="sm" type="submit">
          打开
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          onClick={() => setRefresh((value) => value + 1)}
          disabled={loading}
        >
          <RefreshCwIcon className="size-4" />
          刷新
        </Button>
        <Button
          type="button"
          size="sm"
          variant="outline"
          disabled={!!downloading || !listing}
          onClick={() => void download(path, true)}
        >
          <DownloadIcon className="size-4" />
          当前目录 ZIP
        </Button>
      </form>
      <p className="text-muted-foreground text-xs">
        当前位置：/{listing?.path ?? path}。ZIP 上限 2000 个条目、256
        MiB，跳过符号链接并附导出清单；运行中的文件变化可能中断导出。
      </p>
      {downloading && (
        <div className="flex items-center gap-2 text-sm">
          <Spinner />
          正在下载 {downloading === "zip" ? "目录 ZIP" : downloading}
          <Button variant="ghost" size="sm" onClick={cancelDownload}>
            <XIcon className="size-3" />
            取消下载
          </Button>
        </div>
      )}
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      {incomplete && (
        <p role="status" className="text-amber-600 text-sm">
          本次结果因遍历数量、深度或时间上限而截断，无法代表整个目录。请进入子目录后查看。
        </p>
      )}
      {!!listing?.scan?.skipped_links.length && (
        <p className="text-muted-foreground text-xs">已跳过 {listing.scan.skipped_links.length} 个符号链接。</p>
      )}
      <div className="max-h-[60vh] overflow-auto rounded-md border">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>名称 / 路径</TableHead>
              <TableHead className="w-28">大小</TableHead>
              <TableHead className="hidden w-44 md:table-cell">修改时间</TableHead>
              <TableHead className="w-20">下载</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {loading ? (
              <TableRow>
                <TableCell colSpan={4}>
                  <span className="inline-flex items-center gap-2 py-8">
                    <Spinner />
                    加载中…
                  </span>
                </TableCell>
              </TableRow>
            ) : (
              listing?.entries.map((entry) => (
                <TableRow key={entry.path}>
                  <TableCell>
                    <button
                      type="button"
                      disabled={entry.link}
                      className="flex max-w-full items-start gap-2 text-left disabled:text-muted-foreground"
                      onClick={() => (entry.dir ? navigate(entry.path) : setSelected(entry))}
                    >
                      {entry.dir ? (
                        <FolderIcon className="mt-0.5 size-4 shrink-0" />
                      ) : (
                        <FileTextIcon className="mt-0.5 size-4 shrink-0" />
                      )}
                      <span className="break-all font-mono text-xs">
                        {mode === "recent" ? entry.path : entry.name}
                        {entry.link ? "（符号链接）" : ""}
                      </span>
                    </button>
                  </TableCell>
                  <TableCell className="text-xs tabular-nums">
                    {entry.dir || entry.link ? "—" : fileSize(entry.size)}
                  </TableCell>
                  <TableCell className="hidden text-xs tabular-nums md:table-cell">
                    {new Date(entry.mtime).toLocaleString("zh-CN")}
                  </TableCell>
                  <TableCell>
                    {!entry.dir && !entry.link && (
                      <Button
                        size="icon-sm"
                        variant="ghost"
                        aria-label={`下载 ${entry.path}`}
                        disabled={!!downloading}
                        onClick={() => void download(entry.path)}
                      >
                        <DownloadIcon className="size-4" />
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))
            )}
            {!loading && listing?.entries.length === 0 && (
              <TableRow>
                <TableCell colSpan={4} className="py-8 text-center text-muted-foreground">
                  当前目录暂无文件。
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
      <Sheet
        open={!!selected}
        onOpenChange={(open) => {
          if (!open) setSelected(null);
        }}
      >
        <SheetContent className="w-full! max-w-[56rem]! gap-0 p-0">
          <SheetHeader className="border-b p-5">
            <SheetTitle>文件预览</SheetTitle>
            <SheetDescription className="break-all font-mono">{selected?.path}</SheetDescription>
            <Button
              variant="outline"
              size="sm"
              className="w-fit"
              disabled={!!downloading || !selected}
              onClick={() => selected && void download(selected.path)}
            >
              <DownloadIcon className="size-4" />
              下载文件
            </Button>
          </SheetHeader>
          {previewContent}
        </SheetContent>
      </Sheet>
    </div>
  );
}
