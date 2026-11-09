#!/usr/bin/env pwsh
# devtest.ps1 — 全量测试（真 PostgreSQL）一键跑：Docker 起 PG → 新鲜 gondar_ci 库 →
# GONDAR_PG_DSN=... go test -p 1 ./... -count=1。
#
# 为什么必须新鲜库：测试里有固定名 fixture（唯一键），共享库连跑多轮会撞键产生假失败。
# 为什么 -p 1：跨包并行会死锁（AGENTS.md §1）。
#
# 用法：
#   .\scripts\devtest.ps1                      # 全量（默认新建 gondar_ci）
#   .\scripts\devtest.ps1 -Run TestGraphOverview -Packages ./agent/
#   .\scripts\devtest.ps1 -SkipFresh           # 复用现有库（快速迭代,可能假失败）
param(
    [switch]$SkipFresh,
    [string[]]$Packages = @("./..."),
    [string]$Run = ""
)
# 注意：Windows PowerShell 5.1 里原生命令写 stderr 会变成 NativeCommandError，配合
# ErrorActionPreference=Stop 会直接终止——docker/psql 往 stderr 打正常提示是常态。
# 所以原生调用统一走 cmd /c + $LASTEXITCODE 判定，不靠异常。
$ErrorActionPreference = "Continue"
Set-Location (Join-Path $PSScriptRoot "..")

$Container = "gondar-devtest-pg"
$Port = 15432
$Dsn = "postgres://gondar:gondartest@127.0.0.1:$Port/gondar_ci?sslmode=disable"

function Fail($msg) { Write-Host "[devtest] $msg" -ForegroundColor Red; exit 1 }

# 1) Docker 可用性
docker version --format '{{.Server.Version}}' 2>&1 | Out-Null
if ($LASTEXITCODE -ne 0) {
    Fail "Docker 不可用（需要本机 Docker Desktop/daemon 在跑）。也可手工起库后直接设 GONDAR_PG_DSN 跑 go test。"
}

# 2) 容器：不存在→创建；存在未跑→启动；跑着→复用
$state = (docker inspect -f '{{.State.Running}}' $Container 2>$null)
if ($LASTEXITCODE -ne 0 -or -not $state) {
    Write-Host "[devtest] 创建容器 $Container (postgres:16-alpine :$Port)"
    docker run -d --name $Container -e POSTGRES_PASSWORD=gondartest -e POSTGRES_USER=gondar `
        -e POSTGRES_DB=gondar -p "${Port}:5432" postgres:16-alpine | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "docker run 失败" }
}
elseif ($state.Trim() -ne "true") {
    Write-Host "[devtest] 启动已存在的容器 $Container"
    docker start $Container | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "docker start 失败" }
}

# 3) 等待 PG ready（有界轮询，不用 sleep 猜）
$ready = $false
for ($i = 0; $i -lt 30; $i++) {
    docker exec $Container pg_isready -U gondar -d gondar 2>&1 | Out-Null
    if ($LASTEXITCODE -eq 0) { $ready = $true; break }
    Start-Sleep -Seconds 2
}
if (-not $ready) { Fail "PG 30 次轮询后仍未 ready（docker logs $Container 看原因）" }

# 4) 新鲜库（默认）——先掐断残留连接再 DROP，否则 drop 会被占用挡住
if (-not $SkipFresh) {
    Write-Host "[devtest] 重建 gondar_ci（新鲜库）"
    docker exec $Container psql -U gondar -d postgres -q -c `
        "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='gondar_ci'" | Out-Null
    docker exec $Container psql -U gondar -d postgres -q -c "DROP DATABASE IF EXISTS gondar_ci" | Out-Null
    docker exec $Container psql -U gondar -d postgres -q -c "CREATE DATABASE gondar_ci OWNER gondar" | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "重建 gondar_ci 失败" }
}

# 5) 跑测试
$env:GONDAR_PG_DSN = $Dsn
$goArgs = @("test", "-p", "1") + $Packages + @("-count=1")
if ($Run) { $goArgs += @("-run", $Run) }
Write-Host "[devtest] go $($goArgs -join ' ')  (DSN=gondar_ci@127.0.0.1:$Port)"
& go @goArgs
exit $LASTEXITCODE
