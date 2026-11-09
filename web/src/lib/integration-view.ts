export function formatIntegrationTime(value: string, timezone?: string): string {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "时间无效";
  try {
    return date.toLocaleString("zh-CN", timezone ? { timeZone: timezone } : undefined);
  } catch {
    // The API rejects invalid zones for new schedules. Old or damaged records
    // must still not turn the whole overview into a render-time exception.
    return `${date.toLocaleString("zh-CN", { timeZone: "UTC" })}（时区无效，按 UTC 显示）`;
  }
}
