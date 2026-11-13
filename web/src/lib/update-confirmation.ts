export interface UpdateOperation {
  operation_id: string;
  request_id: string;
  repository: string;
  from_version: string;
  target_version: string;
  asset: string;
  accepted_boot_id: string;
  binary_sha256?: string;
  phase: "accepted" | "staged" | "activating" | "settling" | "completed" | "failed" | "rolled_back";
  error?: string;
  updated_at: number;
}

export interface UpdateStatus {
  operation: UpdateOperation;
  runtime?: { version: string; boot_id: string; binary_sha256: string };
  progress?: { phase: string; percent: number; message: string; operation_id?: string; request_id?: string };
}

export interface UpdateAcknowledgement {
  ok: boolean;
  target: string;
  operation_id?: string;
  request_id?: string;
  operation?: UpdateOperation;
}

export interface ExpectedUpdate {
  request_id: string;
  target_version: string;
  repository: string;
  operation_id?: string;
}

export class UpdateHTTPError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export async function fetchUpdateJSON<T>(path: string, init: RequestInit, token: string | null): Promise<T> {
  const response = await fetch(path, {
    ...init,
    cache: "no-store",
    headers: {
      ...(init.body ? { "Content-Type": "application/json" } : {}),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...init.headers,
    },
  });
  if (!response.ok) {
    let message = `更新状态请求返回 ${response.status}`;
    try {
      const payload = await response.json();
      if (typeof payload.error === "string") message = payload.error;
    } catch {
      /* preserve HTTP status */
    }
    throw new UpdateHTTPError(response.status, message);
  }
  return response.json() as Promise<T>;
}

export const normalizedUpdateVersion = (version: string) => version.replace(/^v/, "");

export function matchesUpdate(status: UpdateStatus, expected: ExpectedUpdate): boolean {
  const op = status.operation;
  return (
    !!op &&
    op.request_id === expected.request_id &&
    (!expected.operation_id || op.operation_id === expected.operation_id) &&
    op.repository === expected.repository &&
    normalizedUpdateVersion(op.target_version) === normalizedUpdateVersion(expected.target_version)
  );
}

export function confirmedUpdate(status: UpdateStatus, expected: ExpectedUpdate): boolean {
  if (!matchesUpdate(status, expected)) return false;
  const { operation: op, runtime } = status;
  return (
    op.phase === "completed" &&
    !!op.operation_id &&
    !!op.accepted_boot_id &&
    !!runtime?.boot_id &&
    runtime.boot_id !== op.accepted_boot_id &&
    normalizedUpdateVersion(runtime.version) === normalizedUpdateVersion(expected.target_version) &&
    !!op.binary_sha256 &&
    runtime.binary_sha256 === op.binary_sha256
  );
}

export type UpdateConfirmationResult =
  | { kind: "confirmed"; version: string }
  | { kind: "failed" | "rolled_back"; message: string }
  | { kind: "auth" | "legacy" | "timeout" | "aborted" };

export function abortableUpdateDelay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
  });
}

// SSE is display-only. Acknowledged updates are observed independently through
// restarts, even when the last progress frame or the POST response is lost.
export async function waitForUpdate(
  expected: ExpectedUpdate,
  signal: AbortSignal,
  readStatus: (expected: ExpectedUpdate, signal: AbortSignal) => Promise<UpdateStatus>,
  options: {
    timeoutMs?: number;
    intervalMs?: number;
    now?: () => number;
    delay?: (ms: number, signal: AbortSignal) => Promise<void>;
    legacyVersion?: (signal: AbortSignal) => Promise<{ version: string }>;
    onStatus?: (status: UpdateStatus) => void;
  } = {},
): Promise<UpdateConfirmationResult> {
  const now = options.now ?? Date.now;
  const deadline = now() + (options.timeoutMs ?? 180_000);
  const delay = options.delay ?? abortableUpdateDelay;
  while (!signal.aborted && now() < deadline) {
    const probe = new AbortController();
    const probeTimer = setTimeout(() => probe.abort(), Math.min(8000, Math.max(1, deadline - now())));
    const probeSignal = AbortSignal.any([signal, probe.signal]);
    try {
      const status = await readStatus(expected, probeSignal);
      if (signal.aborted) return { kind: "aborted" };
      if (matchesUpdate(status, expected)) {
        options.onStatus?.(status);
        if (confirmedUpdate(status, expected))
          return { kind: "confirmed", version: normalizedUpdateVersion(expected.target_version) };
        if (status.operation.phase === "failed" || status.operation.phase === "rolled_back") {
          return { kind: status.operation.phase, message: status.operation.error || "更新未完成" };
        }
      }
    } catch (error) {
      if (signal.aborted) return { kind: "aborted" };
      if (error instanceof UpdateHTTPError && error.status === 401) return { kind: "auth" };
      if (error instanceof UpdateHTTPError && error.status === 404 && options.legacyVersion) {
        try {
          const actual = await options.legacyVersion(probeSignal);
          if (signal.aborted) return { kind: "aborted" };
          if (normalizedUpdateVersion(actual.version) === normalizedUpdateVersion(expected.target_version))
            return { kind: "legacy" };
        } catch (legacyError) {
          if (legacyError instanceof UpdateHTTPError && legacyError.status === 401) return { kind: "auth" };
        }
      }
      // A restart/network failure is not a failed installation or success.
    } finally {
      clearTimeout(probeTimer);
    }
    await delay(options.intervalMs ?? 2000, signal);
  }
  return { kind: signal.aborted ? "aborted" : "timeout" };
}
