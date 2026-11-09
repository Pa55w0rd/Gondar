import type { HostTool, HostToolVersion } from "@/lib/types";

export function usableResolution(version: HostToolVersion | undefined, now = Date.now()): string | undefined {
  return version?.installable &&
    version.resolution_id &&
    version.expires_at &&
    new Date(version.expires_at).getTime() > now
    ? version.resolution_id
    : undefined;
}

export function requireResolution(version: HostToolVersion | undefined, now = Date.now()): string {
  const id = usableResolution(version, now);
  if (!id) throw new Error("版本信息已过期或暂不可用，请重新检测");
  return id;
}

export function missingInstallPlan(tools: HostTool[], versions: Record<string, HostToolVersion>) {
  const eligible = tools.filter(
    (tool) =>
      !tool.available && versions[tool.id]?.available !== true &&
      tool.installable &&
      (tool.kind === "apt" ||
        usableResolution(versions[tool.id]) !== undefined),
  );
  const resolutions: Record<string, string> = {};
  for (const tool of eligible) {
    const id = usableResolution(versions[tool.id]);
    if (id) resolutions[tool.id] = id;
  }
  return { tools: eligible, resolutions };
}

export function toolAvailable(tool: HostTool, version?: HostToolVersion): boolean {
  return tool.available || version?.available === true;
}

export function canUpdate(tool: HostTool, version?: HostToolVersion): boolean {
  return toolAvailable(tool, version) && version?.update_available === true && !!usableResolution(version);
}

export function displayToolVersion(value?: string): string {
  return value && /^[a-f0-9]{40}$/.test(value) ? `提交 ${value.slice(0, 12)}` : value || "未获取";
}
