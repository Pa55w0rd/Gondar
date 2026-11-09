"use client";

import * as React from "react";

import {
  ChevronRightIcon,
  DownloadIcon,
  Loader2Icon,
  MonitorCog,
  PlugIcon,
  RefreshCwIcon,
  TerminalIcon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { api } from "@/lib/api";
import { missingInstallPlan, requireResolution, usableResolution, canUpdate, toolAvailable, displayToolVersion } from "@/lib/host-tool-versions";
import type { HostenvStatus, HostTool, InstallJob, ProxyCheckResult } from "@/lib/types";

import { useToolVersions } from "./use-tool-versions";

const TERMINAL_STATES = new Set(["done", "failed", "cancelled"]);

// 攻击面分类。顺序按攻击流程排：先摸清面(网络/DNS)→ 再打(Web/凭据)
// → 深入(代码/利用/中间件)→ 辅助(隧道/外带/资料/通用)。
const CATEGORY_ORDER: HostTool["category"][] = [
  "network",
  "dns",
  "web",
  "cred",
  "code",
  "exploit",
  "middleware",
  "pivot",
  "oob",
  "material",
  "runtime",
  "",
];

const CATEGORY_LABEL: Record<HostTool["category"], string> = {
  network: "网络与主机",
  dns: "子域名与 DNS",
  web: "Web 应用",
  cred: "凭据与口令",
  code: "代码与密钥",
  exploit: "漏洞利用",
  middleware: "中间件协议",
  pivot: "内网与隧道",
  oob: "外带与回连",
  material: "资料与字典",
  runtime: "通用与运行时",
  "": "其他",
};

// 宿主环境:工具体检。二进制部署形态下系统里装了什么,agent 就能经 Bash
// 直接调什么;此页把家底显式化,缺失项可一键安装(由特权助手 gondar-hostd 执行)。
export default function HostEnvPage() {
  const [inv, setInv] = React.useState<HostenvStatus | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [loadError, setLoadError] = React.useState("");
  const refreshGeneration = React.useRef(0);
  const [proxyInput, setProxyInput] = React.useState("");
  const [savingProxy, setSavingProxy] = React.useState(false);
  const [testingProxy, setTestingProxy] = React.useState(false);
  const [proxyCheck, setProxyCheck] = React.useState<ProxyCheckResult | null>(null);
  // 按工具 id 跟踪安装任务：批量「一键装齐」会把 N 个任务一起塞进助手队列
  // （助手串行执行），所以不再是一个单槽位 job。
  const [jobsByTool, setJobsByTool] = React.useState<Record<string, InstallJob>>({});
  const [selectedJobTool, setSelectedJobTool] = React.useState<string | null>(null);
  const [batchBusy, setBatchBusy] = React.useState(false);
  const [batchSkipped, setBatchSkipped] = React.useState<{ tool_id: string; reason: string }[]>([]);
  const logRef = React.useRef<HTMLPreElement | null>(null);
  const [versionRefresh, setVersionRefresh] = React.useState(0);
  const versionChecks = useToolVersions(
    (inv?.tools ?? [])
      .map((tool) => tool.id)
      .join(","),
    inv?.installer.available ?? false, versionRefresh,
  );

  const refresh = React.useCallback(() => {
    const generation = ++refreshGeneration.current;
    setLoadError("");
    api
      .hostenvStatus()
      .then((s) => {
        if (refreshGeneration.current !== generation) return;
        setInv(s);
        setVersionRefresh((value) => value + 1);
        setProxyInput(s.installer.proxy ?? "");
      })
      .catch((error) => {
        if (refreshGeneration.current === generation)
          setLoadError(error instanceof Error ? error.message : "体检读取失败");
      })
      .finally(() => {
        if (refreshGeneration.current === generation) setLoaded(true);
      });
  }, []);

  React.useEffect(() => {
    refresh();
    return () => {
      refreshGeneration.current++;
    };
  }, [refresh]);

  const activeJobs = React.useMemo(
    () => Object.values(jobsByTool).filter((j) => !TERMINAL_STATES.has(j.state)),
    [jobsByTool],
  );
  const installing = activeJobs.length > 0 || batchBusy;

  // 安装任务轮询：所有进行中的任务每 1.2s 并行拉一次；全部到终态后刷新体检。
  const refreshedAllRef = React.useRef(true);
  React.useEffect(() => {
    if (activeJobs.length === 0) {
      refreshedAllRef.current = true;
      return;
    }
    let live = true;
    let polling = false;
    const timer = setInterval(async () => {
      if (polling) return;
      polling = true;
      const results = await Promise.allSettled(activeJobs.map((j) => api.hostenvJob(j.id)));
      polling = false;
      if (!live) return;
      let sawTerminal = false;
      setJobsByTool((prev) => {
        const next = { ...prev };
        for (const r of results) {
          if (r.status !== "fulfilled") continue; // 轮询失败不打断，下一轮再试
          const j = r.value;
          const before = next[j.tool_id];
          if (!before || before.id !== j.id || TERMINAL_STATES.has(before.state)) continue;
          next[j.tool_id] = j;
          if (TERMINAL_STATES.has(j.state)) {
            sawTerminal = true;
            if (j.state === "done") toast.success(`${j.tool_name} 安装完成`);
            else if (j.state === "failed") toast.error(`${j.tool_name} 安装失败`);
            else toast.info(`${j.tool_name} 安装已取消`);
          }
        }
        return next;
      });
      // 有任务到终态就刷新一次体检（后续轮次里 refresh 会幂等地再对齐）。
      if (sawTerminal) refresh();
    }, 1200);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [activeJobs, refresh]);

  const saveProxy = async () => {
    setSavingProxy(true);
    try {
      await api.setSettings({ install_proxy: proxyInput.trim() });
      toast.success("装机代理已保存");
      refresh();
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "保存失败");
    } finally {
      setSavingProxy(false);
    }
  };

  // 代理会失效，所以改完必须能立刻验证通路——否则只能等装工具失败才发现。
  // 测的是输入框里的值（还没保存），保存与否都能先看清楚对不对。
  const testProxy = async () => {
    setTestingProxy(true);
    setProxyCheck(null);
    try {
      const r = await api.hostenvProxyCheck(proxyInput.trim());
      setProxyCheck(r);
      if (r.ok) toast.success(`代理可用${r.exit_ip ? `（出口 ${r.exit_ip}）` : ""}`);
      else toast.error(`代理不可用：${r.error || r.hint || "未知原因"}`);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "测试失败");
    } finally {
      setTestingProxy(false);
    }
  };

  const install = async (tool: HostTool, resolution?: string) => {
    setBatchBusy(true);
    try {
      const j = await api.hostenvInstall(tool.id, resolution);
      setJobsByTool((prev) => ({ ...prev, [tool.id]: j }));
      setSelectedJobTool(tool.id);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "发起安装失败");
    } finally {
      setBatchBusy(false);
    }
  };

  const installDetected = async (tool: HostTool) => {
    let resolution: string;
    try {
      resolution = requireResolution(versionChecks.versions[tool.id]);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "请重新检测版本");
      return;
    }
    await install(tool, resolution);
  };

  // 一键装齐：把一批工具全部塞进助手队列（助手串行执行，无需等待单个完成）。
  const installMany = async (tools: HostTool[]) => {
    const plan = missingInstallPlan(tools, versionChecks.versions);
    const ids = plan.tools.map((t) => t.id);
    if (ids.length === 0) return;
    setBatchBusy(true);
    try {
      const r = await api.hostenvInstallBatch(ids, plan.resolutions);
      setBatchSkipped(r.skipped);
      setJobsByTool((prev) => {
        const next = { ...prev };
        const byId = new Map(tools.map((t) => [t.id, t]));
        for (const j of r.started) {
          const t = byId.get(j.tool_id);
          if (!t) continue;
          next[j.tool_id] = {
            ...j,
            id: j.job_id,
            tool_id: t.id,
            tool_name: t.name,
            state: "queued",
            exit_code: 0,
            started_at: new Date().toISOString(),
            log: "",
          } as InstallJob;
        }
        return next;
      });
      if (r.started.length > 0) setSelectedJobTool(r.started[0].tool_id);
      if (r.skipped.length > 0) {
        toast.warning(
          `${r.started.length} 项已加入安装队列；${r.skipped.length} 项跳过（${r.skipped
            .slice(0, 3)
            .map((x) => x.tool_id)
            .join("、")}${r.skipped.length > 3 ? " 等" : ""}）`,
        );
      } else {
        toast.success(`${r.started.length} 项已加入安装队列，将依次执行`);
      }
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "批量安装发起失败");
    } finally {
      setBatchBusy(false);
    }
  };

  const cancel = async () => {
    const j = selectedJobTool ? jobsByTool[selectedJobTool] : null;
    if (!j) return;
    try {
      await api.hostenvCancel(j.id);
    } catch {
      // 忽略：任务可能刚好自己结束了
    }
  };

  const available = inv?.tools.filter((t) => t.available) ?? [];
  const missing = inv?.tools.filter((t) => !t.available) ?? [];
  const installerReady = inv?.installer.available ?? false;

  // 按攻击面分组。目录里有近 40 个工具，平铺成一张表没法看——用户要回答的是
  // 「Web 侧我有哪些家伙、缺哪些」，不是「一共有 39 行」。分组顺序固定按攻击流程，
  // 不随可用状态变化，避免刷新后行位置跳动。
  const grouped = React.useMemo(() => {
    const tools = inv?.tools ?? [];
    const byCat = new Map<HostTool["category"], HostTool[]>();
    for (const t of tools) {
      const c = t.category ?? "";
      const arr = byCat.get(c);
      if (arr) arr.push(t);
      else byCat.set(c, [t]);
    }
    const groups: { category: HostTool["category"]; tools: HostTool[] }[] = [];
    for (const c of CATEGORY_ORDER) {
      const ts = byCat.get(c);
      if (ts && ts.length > 0) groups.push({ category: c, tools: ts });
    }
    return groups;
  }, [inv?.tools]);

  // 可一键安装的缺失项：给「一键装齐」用。
  const missingInstallable = React.useMemo(
    () => missingInstallPlan(inv?.tools ?? [], versionChecks.versions).tools,
    [inv?.tools, versionChecks.versions],
  );
  const toolJobState = (id: string): InstallJob["state"] | null => jobsByTool[id]?.state ?? null;
  const selectedJob = selectedJobTool ? (jobsByTool[selectedJobTool] ?? null) : null;
  const selectedLog = selectedJob?.log;
  React.useEffect(() => {
    if (selectedLog === undefined) return;
    if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight;
  }, [selectedLog]);

  // 折叠状态：默认全展开。用户来这里通常是查「缺什么」，藏起来反而添乱。
  const [collapsed, setCollapsed] = React.useState<Set<string>>(new Set());
  const toggleCat = React.useCallback((c: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(c)) next.delete(c);
      else next.add(c);
      return next;
    });
  }, []);

  return (
    <div className="flex flex-col gap-4 p-4 lg:p-6">
      <Card>
        <CardHeader className="flex-row flex-wrap items-center justify-between gap-3">
          <div>
            <CardTitle className="flex items-center gap-2 text-sm">
              <MonitorCog className="size-4" /> 宿主工具体检
            </CardTitle>
            <CardDescription className="mt-1">
              系统里有这些工具,Agent 就能直接调用;派任务选手段时会以这份清单为准。
            </CardDescription>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            {inv && (
              <Badge variant={inv.deploy_mode === "docker" ? "secondary" : "outline"}>
                {inv.deploy_mode === "docker" ? "Docker 容器" : "宿主二进制"}
              </Badge>
            )}
            {missingInstallable.length > 0 && (
              <Button
                size="sm"
                variant="default"
                disabled={!installerReady || installing || versionChecks.checking}
                onClick={() => installMany(missingInstallable)}
              >
                {installing ? <Loader2Icon className="animate-spin" /> : <DownloadIcon />}
                一键安装缺失（{missingInstallable.length}）
              </Button>
            )}
            <Button
              size="sm"
              variant="outline"
              disabled={!installerReady}
              onClick={versionChecks.checking ? versionChecks.cancel : () => void versionChecks.check(true)}
            >
              {versionChecks.checking ? <XIcon /> : <RefreshCwIcon />}
              {versionChecks.checking ? `取消检测（${versionChecks.completed}）` : "检测官方版本"}
            </Button>
            <Button size="sm" variant="outline" onClick={refresh}>
              <RefreshCwIcon /> 刷新
            </Button>
          </div>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {batchSkipped.length > 0 && (
            <div className="rounded-lg border p-3 text-xs" role="status">
              <p className="font-medium">未进入安装队列</p>
              {batchSkipped.map((item) => (
                <p key={item.tool_id} className="break-all">
                  {item.tool_id}：{item.reason}
                </p>
              ))}
            </div>
          )}
          {loadError && (
            <p className="text-sm text-destructive" role="alert">
              体检读取失败：{loadError}
              {inv ? "；当前显示上次结果，请刷新后再确认" : "；请检查服务后重试"}
            </p>
          )}
          {!loaded ? (
            <div className="space-y-2">
              {Array.from({ length: 6 }).map((_, i) => (
                <Skeleton key={i} className="h-8 w-full" />
              ))}
            </div>
          ) : (
            <>
              {/* 装机代理：只作用于安装动作,与目标流量/搜索/LLM 代理无关 */}
              <div className="flex flex-col gap-2 rounded-lg border bg-muted/30 p-3">
                <div className="flex items-center justify-between gap-2">
                  <Label htmlFor="install-proxy" className="text-sm">
                    装机代理
                  </Label>
                  <div className="flex items-center gap-2">
                    {proxyCheck && (
                      <Badge
                        variant={proxyCheck.ok ? "default" : "destructive"}
                        title={proxyCheck.hint || proxyCheck.error || ""}
                      >
                        {proxyCheck.ok
                          ? `可用${proxyCheck.latency_ms ? ` · ${proxyCheck.latency_ms}ms` : ""}`
                          : "不可用"}
                      </Badge>
                    )}
                    <span className="text-muted-foreground text-xs">
                      {installerReady ? "装东西时走它（apt / GitHub 下载）" : "宿主助手未运行，暂不可用"}
                    </span>
                  </div>
                </div>
                <div className="flex items-center gap-2">
                  <Input
                    id="install-proxy"
                    autoComplete="off"
                    className="font-mono text-xs"
                    placeholder="http://10.0.0.14:7897 或 socks5://host:1080（留空=直连）"
                    value={proxyInput}
                    disabled={!loaded || savingProxy}
                    onChange={(e) => {
                      setProxyInput(e.target.value);
                      setProxyCheck(null);
                    }}
                  />
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    onClick={testProxy}
                    disabled={!loaded || testingProxy}
                  >
                    {testingProxy ? <Loader2Icon className="animate-spin" /> : <PlugIcon />}
                    测试
                  </Button>
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    onClick={saveProxy}
                    disabled={!loaded || savingProxy}
                  >
                    保存
                  </Button>
                </div>
                {proxyCheck && !proxyCheck.ok && (
                  <p className="text-destructive text-xs">
                    {proxyCheck.error || "代理不可用"}
                    {proxyCheck.hint ? ` — ${proxyCheck.hint}` : ""}
                  </p>
                )}
                {proxyCheck?.ok && proxyCheck.exit_ip && (
                  <p className="text-muted-foreground text-xs">
                    出口 IP: <span className="font-mono">{proxyCheck.exit_ip}</span>
                  </p>
                )}
                <p className="text-muted-foreground text-xs">
                  国内环境装 nuclei 这类走 GitHub Release 的工具通常需要它；与「全局代理」（Agent
                  目标流量）互不影响。代理随时可改，改完点「测试」确认通路——代理会失效， 装工具失败时先来这里看。
                </p>
              </div>

              {!installerReady && inv?.installer.reason && (
                <div className="rounded-lg border border-amber-500/40 bg-amber-500/10 p-3 text-sm">
                  {inv.installer.reason}
                  {inv.deploy_mode === "docker" && (
                    <span className="text-muted-foreground">
                      {" "}
                      请使用包含同版安装助手的新镜像启动；本页不会连接宿主机Docker接口。
                    </span>
                  )}
                </div>
              )}

              <div className="rounded-lg border bg-muted/20 p-3 text-xs leading-relaxed text-muted-foreground">
                {inv?.deploy_mode === "docker"
                  ? "安装位置：当前Docker容器内，不安装到真实宿主机。受管二进制与Python环境保存在独立工具卷；APT软件保存在容器层，重建容器后需重新安装或写入自建镜像。"
                  : "安装位置：当前运行主程序的系统，由同机安装助手执行。"}
                <p className="mt-1">
                  进入本页自动检测，结果缓存5分钟，手动检测可刷新结果。系统软件源元数据共享缓存30分钟；缺失工具可以安装，已安装工具只在确认有新版本时显示更新。字典按官方仓库提交比较，后台保留来源和下载校验。
                </p>
              </div>

              {installerReady && inv?.installer.upgrade_required && (
                <div className="rounded-lg border border-amber-500/40 bg-amber-500/10 p-3 text-sm">
                  助手版本较旧，不具备自我换装能力，因此无法随主程序自动升级。 在服务器上重跑一次{" "}
                  <code className="font-mono">install.sh</code> 完成引导后， 之后每次发版助手都会自动跟随。
                </div>
              )}

              <p className="text-muted-foreground text-sm">
                可用 <span className="font-medium text-foreground">{available.length}</span> / {inv?.tools.length ?? 0}
                {missing.length > 0 && (
                  <span className="ml-3">
                    缺失 {missing.length} 项{installerReady && "，可一键安装"}
                  </span>
                )}
              </p>

              <div className="flex flex-col gap-3">
                {grouped.map((g) => {
                  const isCollapsed = collapsed.has(g.category);
                  const okCount = g.tools.filter((t) => toolAvailable(t, versionChecks.versions[t.id])).length;
                  return (
                    <div key={g.category} className="rounded-lg border">
                      <div className="flex w-full items-center gap-2 px-3 py-2">
                        <button
                          type="button"
                          onClick={() => toggleCat(g.category)}
                          className="flex flex-1 items-center gap-2 text-left hover:bg-muted/50"
                        >
                          <ChevronRightIcon
                            className={
                              "size-4 shrink-0 text-muted-foreground transition-transform" +
                              (isCollapsed ? "" : "rotate-90")
                            }
                          />
                          <span className="font-medium text-sm">{CATEGORY_LABEL[g.category]}</span>
                          <span className="text-muted-foreground text-xs">
                            {okCount}/{g.tools.length} 可用
                            {g.tools.length - okCount > 0 && (
                              <span className="ml-1 text-amber-600 dark:text-amber-400">
                                缺 {g.tools.length - okCount}
                              </span>
                            )}
                          </span>
                        </button>
                        {installerReady && missingInstallPlan(g.tools, versionChecks.versions).tools.length > 0 && (
                          <Button
                            size="sm"
                            variant="ghost"
                            className="h-7 px-2 text-xs"
                            disabled={installing || versionChecks.checking}
                            onClick={() => installMany(g.tools.filter((t) => !toolAvailable(t, versionChecks.versions[t.id]) && t.installable))}
                          >
                            装齐本组
                          </Button>
                        )}
                      </div>
                      {!isCollapsed && (
                        <Table>
                          <TableHeader>
                            <TableRow>
                              <TableHead className="w-32">工具</TableHead>
                              <TableHead>用途</TableHead>
                              <TableHead className="w-24">状态</TableHead>
                              <TableHead>当前版本 / 最新版本</TableHead>
                              <TableHead className="w-40 text-right">操作</TableHead>
                            </TableRow>
                          </TableHeader>
                          <TableBody>
                            {g.tools.map((t) => (
                              <TableRow key={t.id}>
                                <TableCell className="font-mono text-sm">{t.bin}</TableCell>
                                <TableCell className="text-muted-foreground text-sm">{t.purpose}</TableCell>
                                <TableCell>
                                  {toolAvailable(t, versionChecks.versions[t.id]) ? <Badge>可用</Badge> : <Badge variant="outline">缺失</Badge>}
                                </TableCell>
                                <TableCell className="text-muted-foreground text-xs">
                                  <div className="max-w-lg space-y-1 break-all">
                                    <p>当前版本：{toolAvailable(t, versionChecks.versions[t.id]) ? displayToolVersion(versionChecks.versions[t.id]?.installed_version || "已安装，版本未识别") : "未安装"}</p>
                                    <p>最新版本：{displayToolVersion(versionChecks.versions[t.id]?.latest_version)}</p>
                                    {(versionChecks.errors[t.id] || versionChecks.versions[t.id]?.reason || !versionChecks.versions[t.id]?.latest_version) && <p>{versionChecks.errors[t.id] || versionChecks.versions[t.id]?.reason || (versionChecks.checking ? "正在检测…" : t.kind === "manual" ? "按官方说明维护" : "暂未获取，请重新检测")}</p>}
                                  </div>
                                </TableCell>
                                <TableCell className="text-right">
                                  {!toolAvailable(t, versionChecks.versions[t.id]) && t.installable ? (
                                    <Button size="sm" disabled={!installerReady || installing || (t.kind !== "apt" && !usableResolution(versionChecks.versions[t.id]))}
                                      title={versionChecks.versions[t.id]?.reason || versionChecks.errors[t.id]}
                                      onClick={() => t.kind === "apt" && !usableResolution(versionChecks.versions[t.id]) ? install(t) : installDetected(t)}>
                                      <DownloadIcon />{toolJobState(t.id) === "queued" ? "排队中" : toolJobState(t.id) === "running" ? "安装中" : "安装"}
                                    </Button>
                                  ) : canUpdate(t, versionChecks.versions[t.id]) ? (
                                    <Button size="sm" disabled={!installerReady || installing} onClick={() => installDetected(t)}><DownloadIcon />更新</Button>
                                  ) : toolAvailable(t, versionChecks.versions[t.id]) ? (
                                    <span className="text-muted-foreground text-xs">{versionChecks.versions[t.id]?.state === "current" ? "已是最新" : versionChecks.versions[t.id]?.update_available ? "暂不能更新，请重新检测" : "无需安装"}</span>
                                  ) : <span className="text-muted-foreground text-xs">按官方说明安装</span>}
                                </TableCell>
                              </TableRow>
                            ))}
                          </TableBody>
                        </Table>
                      )}
                    </div>
                  );
                })}
              </div>
            </>
          )}
        </CardContent>
      </Card>

      {/* 安装进度：批量时每个工具一个页签，选中哪个显示哪个的日志 */}
      {selectedJob && (
        <Card>
          <CardHeader className="flex-row items-center justify-between">
            <div className="min-w-0 flex-1">
              <CardTitle className="flex items-center gap-2 text-sm">
                <TerminalIcon className="size-4" /> 安装进度
              </CardTitle>
              <CardDescription className="mt-2 flex flex-wrap items-center gap-1.5">
                {Object.values(jobsByTool)
                  .sort((a, b) => a.tool_name.localeCompare(b.tool_name))
                  .map((j) => (
                    <button
                      key={j.tool_id}
                      type="button"
                      onClick={() => setSelectedJobTool(j.tool_id)}
                      className="focus:outline-none"
                      title={`${j.tool_name} · ${j.state}`}
                    >
                      <Badge
                        variant={
                          j.tool_id === selectedJobTool ? "default" : j.state === "failed" ? "destructive" : "outline"
                        }
                      >
                        {j.state === "running" && <Loader2Icon className="mr-1 size-3 animate-spin" />}
                        {j.tool_name}
                      </Badge>
                    </button>
                  ))}
              </CardDescription>
            </div>
            <div className="flex items-center gap-2">
              {selectedJob && !TERMINAL_STATES.has(selectedJob.state) && (
                <Button size="sm" variant="outline" onClick={cancel}>
                  <XIcon /> 取消
                </Button>
              )}
              {selectedJob && TERMINAL_STATES.has(selectedJob.state) && (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    if (selectedJobTool) {
                      setJobsByTool((prev) => {
                        const next = { ...prev };
                        delete next[selectedJobTool];
                        return next;
                      });
                    }
                    setSelectedJobTool(null);
                  }}
                >
                  关闭
                </Button>
              )}
            </div>
          </CardHeader>
          <CardContent>
            <CardDescription className="mb-2 flex items-center gap-2">
              <span className="font-medium text-foreground">{selectedJob.tool_name}</span>
              {selectedJob.version && (
                <span>
                  {selectedJob.version} · {selectedJob.source}
                </span>
              )}
              <Badge
                variant={
                  selectedJob.state === "done"
                    ? "default"
                    : selectedJob.state === "failed"
                      ? "destructive"
                      : "secondary"
                }
              >
                {selectedJob.state}
              </Badge>
              {selectedJob.error && <span className="text-destructive">{selectedJob.error}</span>}
            </CardDescription>
            <pre
              ref={logRef}
              className="max-h-80 overflow-auto rounded-md bg-muted p-3 font-mono text-xs leading-relaxed"
            >
              {selectedJob.log || "等待输出…（排队中的任务会在轮到它时开始输出）"}
            </pre>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
