"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";

export function ArtifactAssociationHints({
  taskId,
  artifactId,
  onSelect,
}: {
  taskId: string;
  artifactId: number;
  onSelect: (id: string) => void;
}) {
  const [items, setItems] = React.useState<{ finding_id: number; name: string; reason: string }[] | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const generation = React.useRef(0);
  React.useEffect(
    () => () => {
      ++generation.current;
    },
    [], // Parent task/artifact keys remount suggestions when their identity changes.
  );
  const load = async () => {
    const id = ++generation.current;
    setBusy(true);
    setError("");
    setItems(null);
    try {
      const result = await api.artifactFindingSuggestions(taskId, artifactId);
      if (id === generation.current) setItems(result.suggestions);
    } catch (e) {
      if (id === generation.current) setError(e instanceof Error ? e.message : "读取建议失败");
    } finally {
      if (id === generation.current) setBusy(false);
    }
  };
  return (
    <>
      <Button size="sm" variant="outline" disabled={busy} onClick={() => void load()}>
        {busy ? "读取中…" : "查看归属建议"}
      </Button>
      {items && (
        <div className="w-full space-y-1 text-xs">
          <p className="text-muted-foreground">建议依据完整来源 URL；核对附件内容后选择候选，再点击“应用归属”。</p>
          {items.length === 0 && <p>暂无精确 URL 候选，可从漏洞列表中手动选择。</p>}
          {items.map((item) => (
            <Button
              key={item.finding_id}
              size="sm"
              variant="ghost"
              title={item.reason}
              onClick={() => onSelect(String(item.finding_id))}
            >
              选择 #{item.finding_id} {item.name}
            </Button>
          ))}
        </div>
      )}
      {error && (
        <p role="alert" className="w-full text-xs text-destructive">
          {error}
        </p>
      )}
    </>
  );
}
