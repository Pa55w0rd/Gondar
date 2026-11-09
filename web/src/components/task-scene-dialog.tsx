"use client";

import * as React from "react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Textarea } from "@/components/ui/textarea";
import { instantiateTaskScene, taskSceneInput } from "@/lib/task-scene-templates";
import type { TaskTemplate } from "@/lib/types";

export function TaskSceneDialog({
  template,
  onClose,
  onApply,
}: {
  template: TaskTemplate | null;
  onClose: () => void;
  onApply: (template: TaskTemplate) => void;
}) {
  const [input, setInput] = React.useState("");
  const [error, setError] = React.useState("");
  React.useEffect(() => {
    setInput("");
    setError("");
  }, [template?.id]);
  const scene = template ? taskSceneInput(template) : null;
  return (
    <Dialog
      open={template != null}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>使用场景：{template?.name}</DialogTitle>
          <DialogDescription>填写本次对象后生成描述和核验目标；不会立即运行任务。</DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (!template || !scene) return;
            try {
              onApply(instantiateTaskScene(template, input));
            } catch (cause) {
              setError((cause as Error).message);
            }
          }}
          className="space-y-4"
        >
          <Field>
            <FieldLabel htmlFor="scene-targets">{scene?.input_label}</FieldLabel>
            <Textarea
              id="scene-targets"
              value={input}
              onChange={(event) => setInput(event.target.value)}
              placeholder={scene?.input_placeholder}
              className="min-h-32"
              maxLength={8000}
              required
            />
            <FieldDescription>
              {template?.scene ? (
                <>
                  建议时限 {Math.round(template.scene.timeout_seconds / 60)} 分钟；
                  {scene?.seed_first_intent ? "优先直接开跑，复杂说明仍先规划" : "先规划后执行"}。均可在任务高级参数调整。
                </>
              ) : (
                "仅填入模板对象，保留当前任务高级参数。"
              )}
              {(scene?.input_kind === "domains" || scene?.input_kind === "ips") &&
                " 清单会规范化和去重，不自动扩展目标。"}
            </FieldDescription>
          </Field>
          {error && (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              取消
            </Button>
            <Button type="submit">填入任务</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
