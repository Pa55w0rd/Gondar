"use client";

import * as React from "react";
import { startPollSource } from "@/lib/poll-source";

export function usePollSource<T>(load: () => Promise<T>, receive: (value: T) => void, interval: number) {
  const loadRef = React.useRef(load);
  const receiveRef = React.useRef(receive);
  loadRef.current = load;
  receiveRef.current = receive;
  const sourceRef = React.useRef<ReturnType<typeof startPollSource<T>> | null>(null);
  React.useEffect(() => {
    const source = startPollSource(
      () => loadRef.current(),
      (value) => receiveRef.current(value),
      interval,
    );
    sourceRef.current = source;
    return () => {
      source.stop();
      sourceRef.current = null;
    };
  }, [interval]);
  return React.useCallback(() => sourceRef.current?.refresh(), []);
}
