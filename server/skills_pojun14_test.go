package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"
	"gondar/agent"
	"gondar/db"
)

func TestPojun14SkillsReachRuntimeAndHonorOptOut(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("未配置专用测试库：%v", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	// No subprocess, online MCP or model is part of this skills-only test.
	rows, err := pg.Query(`SELECT id FROM mcp_servers WHERE enabled=true`)
	if err != nil {
		t.Fatal(err)
	}
	var enabledMCP []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		enabledMCP = append(enabledMCP, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if _, err := pg.Exec(`UPDATE mcp_servers SET enabled=false WHERE enabled=true`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, id := range enabledMCP {
			if _, err := pg.Exec(`UPDATE mcp_servers SET enabled=true WHERE id=$1`, id); err != nil {
				t.Error(err)
			}
		}
	}()
	old := agent.ToolAugment
	defer func() { agent.ToolAugment = old }()
	wireAgentAugment(pg, filepath.Join("..", "skills"), nil)
	session := fmt.Sprintf("pojun14-skill-runtime-%d", time.Now().UnixNano())
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{SessionID: session})
	defer func() {
		if _, err := pg.Exec(`DELETE FROM skill_usage WHERE session_id=$1`, session); err != nil {
			t.Error(err)
		}
	}()
	findSkill := func() actool.CoreTool {
		t.Helper()
		tools, _, cleanup := agent.ToolAugment(ctx, "worker")
		defer cleanup()
		for _, tool := range tools {
			if tool.Name() == "Skill" {
				return tool
			}
		}
		t.Fatal("实际Worker工具集合没有Skill入口")
		return nil
	}
	meta := findSkill()
	for _, name := range []string{
		"pentest-verification", "source-code-hunting", "api-security-testing", "ai-llm-app-attack",
		"component-vuln-intel", "pentest-output-standards", "redteam-opsec", "web-attack-methods",
	} {
		if !strings.Contains(meta.Prompt(), name) {
			t.Errorf("技能只有文件但未向Worker列出：%s", name)
		}
		input, _ := json.Marshal(map[string]string{"name": name})
		result, err := meta.Call(ctx, input, nil)
		if err != nil || result.IsError || len(result.Extra) != 1 {
			t.Errorf("实际技能调用未载入指导正文：%s，%+v，%v", name, result, err)
		}
	}
	worker, err := pg.GetAgentByKey("worker")
	if err != nil || worker == nil {
		t.Fatalf("worker不存在：%v", err)
	}
	var original bool
	if err := pg.QueryRow(`SELECT enabled FROM agent_skill_visibility WHERE agent_id=$1 AND skill_name='pentest-verification'`, worker.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pg.ToggleSkillVisibility(worker.ID, "pentest-verification", original); err != nil {
			t.Error(err)
		}
	}()
	if err := pg.ToggleSkillVisibility(worker.ID, "pentest-verification", false); err != nil {
		t.Fatal(err)
	}
	meta = findSkill()
	if strings.Contains(meta.Prompt(), "pentest-verification") {
		t.Error("显式关闭的技能仍在Worker运行提示中")
	}
	result, err := meta.Call(ctx, json.RawMessage(`{"name":"pentest-verification"}`), nil)
	if err != nil || !result.IsError {
		t.Fatalf("显式关闭的技能仍能经实际Skill入口调用：%+v，%v", result, err)
	}
}
