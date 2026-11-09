"use client";

import * as React from "react";

import { api } from "@/lib/api";
import type { HostToolVersion } from "@/lib/types";

// Discovery is cancellable, read-only and bounded to four requests in flight.
// A generation also covers clients whose fetch implementation ignores abort.
export function useToolVersions(ids: string, enabled: boolean, refresh = 0) {
  const [versions, setVersions] = React.useState<Record<string, HostToolVersion>>({});
  const [errors, setErrors] = React.useState<Record<string, string>>({});
  const [checking, setChecking] = React.useState(false);
  const [completed, setCompleted] = React.useState(0);
  const controller = React.useRef<AbortController | null>(null);
  const generation = React.useRef(0);
  const cancel = React.useCallback(() => {
    generation.current++;
    controller.current?.abort();
    setChecking(false);
  }, []);
  const check = React.useCallback(async (force = false) => {
    controller.current?.abort();
    const token = ++generation.current;
    const abort = new AbortController();
    controller.current = abort;
    const list = ids ? ids.split(",") : [];
    setChecking(true);
    setCompleted(0);
    setErrors({});
    setVersions({});
    let cursor = 0;
    await Promise.all(
      Array.from({ length: Math.min(4, list.length) }, async () => {
        while (!abort.signal.aborted && cursor < list.length) {
          const id = list[cursor++];
          try {
            const version = await api.hostenvVersion(id, abort.signal, force);
            if (generation.current === token) setVersions((old) => ({ ...old, [id]: version }));
          } catch (error) {
            if (generation.current === token && !abort.signal.aborted) {
              setErrors((old) => ({ ...old, [id]: error instanceof Error ? error.message : "检测失败" }));
            }
          } finally {
            if (generation.current === token) setCompleted((old) => old + 1);
          }
        }
      }),
    );
    if (generation.current === token) setChecking(false);
  }, [ids]);
  React.useEffect(() => {
    if (enabled) void check();
    return () => {
      generation.current++;
      controller.current?.abort();
    };
  }, [enabled, check, refresh]);
  return { versions, errors, checking, completed, check, cancel };
}
