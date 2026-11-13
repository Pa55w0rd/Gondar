export interface PollEnvironment {
  visible: () => boolean;
  listen: (changed: () => void) => () => void;
  after: (run: () => void, ms: number) => () => void;
}

const browserEnvironment: PollEnvironment = {
  visible: () => typeof document === "undefined" || document.visibilityState !== "hidden",
  listen: (changed) => {
    if (typeof document === "undefined") return () => {};
    document.addEventListener("visibilitychange", changed);
    return () => document.removeEventListener("visibilitychange", changed);
  },
  after: (run, ms) => {
    const timer = setTimeout(run, ms);
    return () => clearTimeout(timer);
  },
};

// Each source owns one in-flight request and one timer. Slow or failed sources
// never hold up their peers. An interval of zero enables only initial/manual
// refresh (and one refresh when returning to the visible page).
export function startPollSource<T>(
  load: () => Promise<T>,
  receive: (value: T) => void,
  interval: number,
  environment = browserEnvironment,
) {
  let stopped = false;
  let running: Promise<void> | null = null;
  let pending = false;
  let clearTimer: (() => void) | undefined;

  const schedule = () => {
    clearTimer?.();
    clearTimer = undefined;
    if (!stopped && interval > 0 && environment.visible()) {
      clearTimer = environment.after(() => void refresh(), interval);
    }
  };

  const refresh = (): Promise<void> => {
    if (stopped) return Promise.resolve();
    if (!environment.visible()) return running ?? Promise.resolve();
    if (running) {
      pending = true; // Coalesce manual/visibility refreshes into one follow-up.
      return running;
    }
    pending = false;
    clearTimer?.();
    clearTimer = undefined;
    running = Promise.resolve()
      .then(load)
      .then((value) => {
        if (!stopped) receive(value);
      })
      .catch(() => {
        // Keep the last successful value; the source retries independently.
      })
      .finally(() => {
        running = null;
        if (stopped) return;
        if (pending && environment.visible()) void refresh();
        else schedule();
      });
    return running;
  };

  const unsubscribe = environment.listen(() => {
    if (environment.visible()) void refresh();
    else {
      clearTimer?.();
      clearTimer = undefined;
    }
  });
  void refresh();
  return {
    refresh,
    stop: () => {
      stopped = true;
      pending = false;
      clearTimer?.();
      clearTimer = undefined;
      unsubscribe();
    },
  };
}
