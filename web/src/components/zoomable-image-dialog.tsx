"use client";

import * as React from "react";
import { MinusIcon, PlusIcon, RotateCcwIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";

const initial = { scale: 1, x: 0, y: 0 };
type View = typeof initial;

// 按机制适配 Pojun v0.4.48/.53；来源和交互取舍见本版变更记录。
export function ZoomableImageDialog({ open, onClose, src, title, description, downloadUrl, footer }: {
  open: boolean;
  onClose: () => void;
  src: string;
  title: string;
  description?: string;
  downloadUrl?: string;
  footer?: React.ReactNode;
}) {
  const [view, setView] = React.useState<View>(initial);
  const [failed, setFailed] = React.useState(false);
  const [dragging, setDragging] = React.useState(false);
  const [viewport, setViewport] = React.useState<HTMLElement | null>(null);
  const drag = React.useRef<{ pointer: number; x: number; y: number; base: View } | null>(null);
  const changeScale = React.useCallback((factor: number) => {
    setView((current) => ({ ...current, scale: Math.max(0.5, Math.min(8, current.scale * factor)) }));
  }, []);

  React.useEffect(() => {
    if (!open || !src) return;
    setView(initial);
    setFailed(false);
    setDragging(false);
    drag.current = null;
  }, [open, src]);

  // React 委托的 wheel 事件可能是 passive；只在当前视口注册可取消的监听。
  React.useEffect(() => {
    const element = viewport;
    if (!open || !element) return;
    const wheel = (event: WheelEvent) => {
      if (event.deltaY === 0) return;
      event.preventDefault();
      changeScale(event.deltaY < 0 ? 1.15 : 1 / 1.15);
    };
    element.addEventListener("wheel", wheel, { passive: false });
    return () => element.removeEventListener("wheel", wheel);
  }, [viewport, open, changeScale]);

  function stopDrag(event: React.PointerEvent<HTMLElement>) {
    if (drag.current?.pointer !== event.pointerId) return;
    drag.current = null;
    setDragging(false);
    if (event.currentTarget.hasPointerCapture(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
  }

  return (
    <Dialog open={open} onOpenChange={(next) => { if (!next) onClose(); }}>
      <DialogContent className="flex max-h-[94dvh] min-w-0 flex-col overflow-y-auto sm:max-w-5xl">
        <DialogHeader>
          <DialogTitle className="break-all pr-6 text-base">{title}</DialogTitle>
          <DialogDescription className="break-all">{description ?? "滚轮或按钮缩放，放大后拖动；双击或按 0 还原。"}</DialogDescription>
        </DialogHeader>
        <section
          ref={setViewport}
          aria-label="图片缩放预览"
          // biome-ignore lint/a11y/noNoninteractiveTabindex: 此视口提供+/-/0键盘缩放与还原，需要可独立聚焦。
          tabIndex={0}
          className="relative flex h-[60dvh] min-h-48 min-w-0 touch-none items-center justify-center overflow-hidden rounded-md border bg-muted/30 outline-offset-2"
          style={{ cursor: view.scale > 1 ? dragging ? "grabbing" : "grab" : "default" }}
          onDoubleClick={() => setView(initial)}
          onKeyDown={(event) => {
            if (event.key === "+" || event.key === "=") { event.preventDefault(); changeScale(1.25); }
            if (event.key === "-") { event.preventDefault(); changeScale(0.8); }
            if (event.key === "0") { event.preventDefault(); setView(initial); }
          }}
          onPointerDown={(event) => {
            if (view.scale <= 1 || drag.current || event.button !== 0) return;
            drag.current = { pointer: event.pointerId, x: event.clientX, y: event.clientY, base: view };
            event.currentTarget.setPointerCapture(event.pointerId);
            setDragging(true);
          }}
          onPointerMove={(event) => {
            const start = drag.current;
            if (start?.pointer !== event.pointerId) return;
            setView({ ...start.base, x: start.base.x + event.clientX - start.x, y: start.base.y + event.clientY - start.y });
          }}
          onPointerUp={stopDrag}
          onPointerCancel={stopDrag}
          onLostPointerCapture={() => { drag.current = null; setDragging(false); }}
        >
          {failed ? <p role="alert" className="p-4 text-sm">图片无法载入，可下载原件核对。</p> : (
            // eslint-disable-next-line @next/next/no-img-element
            <img key={src} src={src} alt={title} draggable={false} onError={() => setFailed(true)}
              className="pointer-events-none max-h-full max-w-full select-none object-contain"
              style={{ transform: `translate(${view.x}px, ${view.y}px) scale(${view.scale})` }} />
          )}
        </section>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <Button size="icon-sm" variant="outline" aria-label="缩小图片" disabled={view.scale <= 0.5} onClick={() => changeScale(0.8)}><MinusIcon /></Button>
            <output aria-label="缩放比例" className="w-12 text-center text-xs tabular-nums">{Math.round(view.scale * 100)}%</output>
            <Button size="icon-sm" variant="outline" aria-label="放大图片" disabled={view.scale >= 8} onClick={() => changeScale(1.25)}><PlusIcon /></Button>
            <Button size="sm" variant="ghost" onClick={() => setView(initial)}><RotateCcwIcon />还原</Button>
          </div>
          {downloadUrl && <Button size="sm" variant="outline" asChild><a href={downloadUrl}>下载原件</a></Button>}
        </div>
        {footer}
      </DialogContent>
    </Dialog>
  );
}
