"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { api } from "@/lib/api";
import type { ArtifactContentPreview } from "@/lib/integration-types";

export function ArtifactContentPreviewButton({
  taskId,
  artifactId,
  name,
}: {
  taskId: string;
  artifactId: number;
  name: string;
}) {
  const [open, setOpen] = React.useState(false);
  const [view, setView] = React.useState<ArtifactContentPreview | null>(null);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [selectedEntry, setSelectedEntry] = React.useState("");
  const generation = React.useRef(0);
  React.useEffect(
    () => () => {
      ++generation.current;
    },
    [], // Parent task/artifact keys remount this view when its identity changes.
  );
  const load = async (entry = "") => {
    const id = ++generation.current;
    setSelectedEntry(entry);
    setBusy(true);
    setError("");
    setView(null);
    try {
      const result = await api.artifactContentPreview(taskId, artifactId, entry);
      if (generation.current === id) setView(result);
    } catch (e) {
      if (generation.current === id) setError(e instanceof Error ? e.message : "读取失败");
    } finally {
      if (generation.current === id) setBusy(false);
    }
  };
  return (
    <>
      <Button
        size="sm"
        variant="ghost"
        onClick={() => {
          setOpen(true);
          void load();
        }}
      >
        内容预览
      </Button>
      <Dialog
        open={open}
        onOpenChange={(value) => {
          setOpen(value);
          if (!value) ++generation.current;
        }}
      >
        <DialogContent className="max-w-4xl">
          <DialogHeader>
            <DialogTitle className="break-all">{view?.name || name}</DialogTitle>
          </DialogHeader>
          <p className="text-xs text-muted-foreground">预览最多显示 64 KiB、60 行；完整证据可下载原件。</p>
          {busy && <p role="status">正在读取…</p>}
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          {view && (
            <div className="max-h-[65vh] overflow-auto rounded border p-3">
              {view.entries && (
                <div className="space-y-1">
                  {view.entries.map((entry, index) => (
                    <button
                      key={`${index}-${entry.name}`}
                      type="button"
                      disabled={entry.directory}
                      onClick={() => void load(entry.name)}
                      className="flex w-full justify-between gap-3 rounded px-2 py-1 text-left text-sm hover:bg-muted disabled:text-muted-foreground"
                    >
                      <span className="break-all">{entry.name}</span>
                      <span className="shrink-0">{entry.size_bytes.toLocaleString()} 字节</span>
                    </button>
                  ))}
                </div>
              )}
              {view.rows && (
                <table className="text-xs">
                  <tbody>
                    {view.rows.map((row, i) => (
                      <tr key={i}>
                        {row.map((cell, j) => (
                          <td key={j} className="max-w-96 whitespace-pre-wrap break-words border px-2 py-1 align-top">
                            {cell}
                          </td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {view.text !== undefined && <pre className="whitespace-pre-wrap break-all text-xs">{view.text}</pre>}
            </div>
          )}
          {view?.truncated && (
            <p className="text-sm text-amber-700 dark:text-amber-400">
              当前只显示部分内容，未显示的内容仍保留在原件中。
            </p>
          )}
          {view?.notice && <p className="text-xs text-muted-foreground">{view.notice}</p>}
          <div className="flex gap-2">
            {selectedEntry && (
              <Button variant="outline" onClick={() => void load()}>
                返回归档目录
              </Button>
            )}
            <Button variant="outline" asChild>
              <a href={`${api.artifactUrl(taskId, artifactId)}?download=1`}>下载原件</a>
            </Button>
          </div>
        </DialogContent>
      </Dialog>
    </>
  );
}
