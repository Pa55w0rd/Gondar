package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"gondar/config"
	"gondar/doctor"
	"gondar/hostd"
	"gondar/hosttools"
)

func runDoctor(args []string, out, errors io.Writer) int {
	flags := flag.NewFlagSet("gondar doctor", flag.ContinueOnError)
	flags.SetOutput(errors)
	jsonOutput := flags.Bool("json", false, "输出 JSON")
	data := flags.String("data", filepath.Join(config.BaseDir(), "data"), "只读检查的数据目录")
	cfg := flags.String("config", config.Path(), "只读检查的配置文件")
	hash := flags.Bool("hash-tools", false, "读取本地工具文件 SHA256，不执行工具")
	probe := flags.Bool("probe-versions", false, "执行固定的本地版本参数；不会安装或下载")
	toolLock := flags.String("tool-lock", hostd.DefaultToolLockPath, "管理员审核的工具安装清单")
	template := flags.Bool("tool-lock-template", false, "只输出待审核的工具安装清单模板，不检查或安装")
	target := flags.String("target", "linux/amd64", "安装清单目标，如 linux/amd64 或 linux/arm64")
	manifest := flags.String("release-manifest", "", "离线发行 SHA256SUMS")
	signature := flags.String("release-signature", "", "离线 SHA256SUMS.sig")
	archive := flags.String("release-archive", "", "仅做可信签名和摘要核对的本地归档")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(errors, "doctor 不接受额外命令或安装脚本")
		return 2
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if *template {
		parts := strings.Split(*target, "/")
		if len(parts) != 2 || parts[0] != "linux" || (parts[1] != "amd64" && parts[1] != "arm64") {
			fmt.Fprintln(errors, "安装清单目标需要 linux/amd64 或 linux/arm64")
			return 2
		}
		if err := encoder.Encode(hosttools.InstallLockTemplate(parts[0], parts[1])); err != nil {
			return 1
		}
		return 0
	}
	dsn := config.InspectionPostgresDSN(*cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report := doctor.Run(ctx, doctor.Options{Version: version, ConfigPath: *cfg, DataDir: *data, DSN: dsn, HashTools: *hash, ProbeVersions: *probe, ToolLockPath: *toolLock, ReleaseManifest: *manifest, ReleaseSignature: *signature, ReleaseArchive: *archive})
	if *jsonOutput {
		if err := encoder.Encode(report); err != nil {
			return 1
		}
	} else {
		mode := "只读静态检查"
		if !report.ReadOnly {
			mode = "启用固定本地版本进程探测"
		}
		fmt.Fprintf(out, "Gondar %s 部署检查（%s）\nGo %s · %s/%s\n", report.Version, mode, report.GoVersion, report.OS, report.Arch)
		for _, item := range []struct {
			name  string
			check doctor.Check
		}{{"配置", report.Config}, {"PostgreSQL", report.Postgres}, {"磁盘", report.Disk.Check}, {"工具安装锁", report.ToolLock}, {"离线发行验证", report.ReleaseCheck}} {
			fmt.Fprintf(out, "[%s] %s：%s\n", item.check.Status, item.name, item.check.Detail)
		}
		fmt.Fprintf(out, "磁盘空闲：%d MiB\n", report.Disk.FreeBytes>>20)
		fmt.Fprintf(out, "可信更新就绪：%t；%s\n", report.UpdatePolicy.Ready, report.UpdatePolicy.Reason)
		for _, tool := range report.Tools {
			fmt.Fprintf(out, "工具 %s：存在=%t 版本=%s（%s）\n", tool.ID, tool.Available, tool.Version, tool.VersionSource)
		}
	}
	return report.ExitCode()
}
