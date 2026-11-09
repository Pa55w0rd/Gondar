export interface ArtifactContentPreview {
  format: string;
  name: string;
  size_bytes: number;
  text?: string;
  rows?: string[][];
  entries?: { name: string; size_bytes: number; directory: boolean }[];
  truncated: boolean;
  notice?: string;
}

export interface DailySchedule {
  id: number;
  source_task_id: number;
  daily_at: string;
  timezone: string;
  enabled: boolean;
  max_runs: number;
  runs_created: number;
  timeout_seconds: number;
  next_run_at: string;
  last_task_id?: number;
  last_error?: string;
  skipped: number;
  created_at: string;
}

export interface TaskLesson {
  id: number;
  task_id: number;
  tool: string;
  summary: string;
  resolution: string;
  tags: string[];
  evidence?: { activity_id: number; tool: string; sha256: string; is_error: boolean; excerpt: string }[];
  status: "pending" | "accepted" | "rejected" | "retired";
  revision: number;
  shared: boolean;
  origin: string;
  review_note?: string;
  expires_at: string;
  created_at: string;
  updated_at: string;
}

export interface TaskResource {
  id: number;
  task_id: number;
  asset_id?: number;
  kind: "local_file" | "upload" | "account" | "session" | "listener";
  name: string;
  state: "planned" | "created" | "active" | "cleanup_pending" | "cleaned" | "failed";
  revision: number;
  note: string;
  cleanup: string;
  evidence_ids: number[];
  origin: string;
  expires_at: string;
  updated_at: string;
}
