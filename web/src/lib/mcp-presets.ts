export type MCPPreset = {
  id: string;
  title: string;
  description: string;
  transport: "stdio" | "http" | "sse";
  command: string;
  args: string[];
  url: string;
  source: string;
};

// 仅填充配置；保存为禁用，不调用包管理器或连接远端。
export const MCP_PRESETS: readonly MCPPreset[] = [
  { id: "playwright", title: "Playwright 浏览器", description: "使用已安装的 playwright-mcp；镜像固定版本0.0.84。隔离浏览器配置目录，不复用个人登录。", transport: "stdio", command: "playwright-mcp", args: ["--headless", "--isolated", "--browser", "chromium"], url: "", source: "https://github.com/microsoft/playwright-mcp" },
  { id: "chrome-devtools", title: "Chrome DevTools", description: "需先安装审核过的固定版本和Chrome；每个实例使用隔离配置。", transport: "stdio", command: "chrome-devtools-mcp", args: ["--headless", "--isolated"], url: "", source: "https://github.com/ChromeDevTools/chrome-devtools-mcp" },
  { id: "burp", title: "Burp Suite", description: "需自行安装扩展并核对实际监听地址；Docker内127.0.0.1指当前容器。", transport: "sse", command: "", args: [], url: "http://127.0.0.1:9876/sse", source: "https://github.com/PortSwigger/mcp-server" },
  { id: "fetch", title: "Fetch 文档读取", description: "使用已安装的mcp-server-fetch，不自动uvx下载；只读取任务需要的资料。", transport: "stdio", command: "mcp-server-fetch", args: [], url: "", source: "https://github.com/modelcontextprotocol/servers" },
  { id: "filesystem", title: "Filesystem 授权目录", description: "需已安装的mcp-server-filesystem；必须填写具体任务目录，默认不开放整个data或系统根。", transport: "stdio", command: "mcp-server-filesystem", args: ["<填写授权目录>"], url: "", source: "https://github.com/modelcontextprotocol/servers" },
];

export function parseMCPArguments(text: string): string[] {
  const trimmed = text.trim();
  const args: unknown = trimmed.startsWith("[") ? JSON.parse(trimmed) : trimmed ? trimmed.split(/\s+/) : [];
  if (!Array.isArray(args) || args.length > 128 || args.some((value) => typeof value !== "string" || value.includes("\0") || value.length > 32768)) {
    throw new Error("参数应为最多128项的字符串数组");
  }
  return args as string[];
}
