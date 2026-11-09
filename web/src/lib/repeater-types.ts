import type { AssetInterceptRule } from "@/lib/types";

export interface RepeaterPolicy {
  allow_rules: AssetInterceptRule[];
  capture_enabled: boolean;
  request_limit: number;
  response_limit: number;
  redirects_followed: false;
  run_limit: number;
  storage_limit: number;
  run_count: number;
  reserved_bytes: number;
}

export interface RepeaterInput {
  task_id: string;
  origin: string;
  raw_request: string;
  source_traffic_id?: string;
  timeout_seconds: number;
}

export interface RepeaterRun {
  id: number;
  task_id: number;
  source_traffic_id?: string;
  traffic_id?: string;
  url: string;
  method: string;
  request?: string;
  state: "running" | "completed" | "blocked" | "canceled" | "error";
  status: number;
  response_head?: string;
  response_body_base64?: string;
  response_sha256: string;
  response_bytes: number;
  truncated: boolean;
  error?: string;
  capture_note?: string;
  elapsed_ms: number;
  created_at: string;
  completed_at?: string;
}
