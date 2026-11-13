export interface LLMTestRequest {
  profile_id?: number;
  provider?: string;
  format?: string;
  model?: string;
  base_url?: string;
  api_key?: string;
  proxy?: string;
  thinking_type?: string;
  reasoning_effort?: string;
  streaming?: boolean;
  session_header_key?: string;
  max_tokens?: number;
  max_tokens_field?: string;
  context_window_k?: number;
  rate_per_second?: number;
  rate_per_minute?: number;
  max_concurrent?: number;
}

// Object calls preserve omissions; positional calls keep the existing UI contract.
export function llmTestRequest(
  provider: string | LLMTestRequest,
  model?: string,
  base_url?: string,
  api_key?: string,
  proxy = "",
  thinking_type = "",
  reasoning_effort = "",
  profile_id?: number,
  streaming = true,
  session_header_key = "",
): LLMTestRequest {
  if (typeof provider !== "string") return { ...provider };
  return {
    provider,
    model,
    base_url,
    api_key,
    proxy,
    thinking_type,
    reasoning_effort,
    profile_id,
    streaming,
    session_header_key,
  };
}
