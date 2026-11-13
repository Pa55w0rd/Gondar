"use client";

import Link from "next/link";

import { CopyButton } from "@/components/copy-button";
import type { FindingReportSource as ReportSource } from "@/lib/types";

export function FindingReportSource({ source }: { source?: ReportSource }) {
  if (!source) return <p className="text-muted-foreground text-xs">报告来源未绑定。</p>;
  return (
    <div className="flex flex-col gap-1 text-muted-foreground text-xs">
      <div className="flex flex-wrap items-center gap-2">
        {source.kind === "conversation" && source.available && source.conversation_id ? (
          <Link
            className="underline underline-offset-2 hover:text-foreground"
            href={`/chat?c=${encodeURIComponent(source.conversation_id)}`}
          >
            查看报告来源会话 #{source.conversation_id}
          </Link>
        ) : (
          <span>
            {source.kind === "recovery_job" ? `后台恢复作业 · 第 ${source.recovery_attempt ?? "?"} 次` : "报告来源会话"}
          </span>
        )}
        <span>依据证据版本：{source.evidence_version < 0 ? "未绑定" : source.evidence_version}</span>
        <CopyButton
          text={JSON.stringify(source, null, 2)}
          successMessage="已复制报告来源回执"
          variant="ghost"
          className="h-6 px-2 text-xs"
        />
      </div>
      {!source.available && <span>{source.unavailable_reason || "来源身份无法核对，当前不提供会话链接。"}</span>}
      <details>
        <summary className="cursor-pointer">来源回执</summary>
        <pre className="mt-1 overflow-auto whitespace-pre-wrap break-all rounded border p-2">
          {JSON.stringify(source, null, 2)}
        </pre>
      </details>
    </div>
  );
}
