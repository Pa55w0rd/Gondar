import type { TaskScenePreset, TaskTemplate } from "@/lib/types";

function domainName(raw: string): string {
  if (/[\s/:?#@\\*%]/.test(raw)) throw new Error("域名清单每行只填精确主机名，不含通配、URL路径或端口");
  const host = new URL(`https://${raw}`).hostname.toLowerCase().replace(/\.$/, "");
  if (!host || /^[0-9.]+$/.test(host) || host.includes(":")) throw new Error("请填写域名，IP请使用批量IP场景");
  if (!host.split(".").every((label) => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label))) {
    throw new Error("域名格式无效");
  }
  return host;
}

function ipAddress(raw: string): string {
  if (raw.includes("/")) throw new Error("请填写精确IP；不会自动展开CIDR或网段");
  if (raw.includes(":")) {
    const value = raw.replace(/^\[|\]$/g, "");
    const host = new URL(`http://[${value}]`).hostname;
    return host.slice(1, -1);
  }
  const parts = raw.split(".");
  if (parts.length !== 4 || !parts.every((part) => /^(0|[1-9][0-9]{0,2})$/.test(part) && Number(part) <= 255)) {
    throw new Error("IP格式无效，请使用IPv4点分十进制或完整IPv6地址");
  }
  return parts.map(Number).join(".");
}

export function normalizeSceneTargets(scene: TaskScenePreset, input: string): string[] {
  const raw = input.trim();
  if (!raw) throw new Error("请填写场景对象");
  if (raw.length > 8000 || /\{\{[^}]*\}\}/.test(raw)) throw new Error("场景对象过长或仍含未填写占位符");
  if (scene.input_kind === "text") return [raw];
  if (scene.input_kind === "host") {
    if (/\s/.test(raw) || raw.includes("*")) throw new Error("单域名场景只接受一个精确主机或HTTP(S)入口URL");
    const hasScheme = /^[a-z][a-z0-9+.-]*:\/\//i.test(raw);
    const url = new URL(hasScheme ? raw : `https://${raw}`);
    if (!["http:", "https:"].includes(url.protocol) || url.username || url.password) {
      throw new Error("入口须为HTTP(S)，且不能在URL中携带账号密码");
    }
    domainName(url.hostname);
    url.hostname = url.hostname.toLowerCase().replace(/\.$/, "");
    url.hash = "";
    return [url.href];
  }
  const values = raw
    .split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean);
  const targets = [...new Set(values.map(scene.input_kind === "domains" ? domainName : ipAddress))];
  if (targets.length === 0 || targets.length > scene.max_targets) {
    throw new Error(`每次最多 ${scene.max_targets} 个去重后的目标`);
  }
  return targets;
}

// Rendering is literal replacement, not execution of a template language.
export function taskTemplateNeedsTarget(template: TaskTemplate): boolean {
  return template.description.includes("{{target}}") || template.goal.includes("{{target}}");
}

export function taskSceneInput(template: TaskTemplate): TaskScenePreset {
  return (
    template.scene ?? {
      input_label: "本次模板对象",
      input_placeholder: "填写目标或范围说明",
      input_kind: "text",
      max_targets: 1,
      timeout_seconds: 0,
      seed_first_intent: false,
      coverage_enabled: true,
    }
  );
}

export function instantiateTaskScene(template: TaskTemplate, input: string): TaskTemplate {
  if (!taskTemplateNeedsTarget(template)) throw new Error("该模板没有目标占位符");
  const targets = normalizeSceneTargets(taskSceneInput(template), input).join("\n");
  return {
    ...template,
    description: template.description.replaceAll("{{target}}", () => targets),
    goal: template.goal.replaceAll("{{target}}", () => targets),
  };
}
