package server

import (
	"encoding/json"
	"gondar/agent"
	"gondar/db"
	"log"
)

func refreshExecutionToolContracts(pg *db.DB) error {
	return refreshCodeToolContracts(pg, agent.BuiltinToolSeeds())
}

func refreshCodeToolContracts(pg *db.DB, seeds []agent.ToolSeed) error {
	// Every code-defined contract is checked at startup, including previously
	// seeded databases. A version flag cannot silently hide a newly added field.
	rows, err := pg.ListTools()
	if err != nil {
		return err
	}
	byKey := make(map[string]*db.Tool, len(rows))
	for _, row := range rows {
		byKey[row.Key] = row
	}
	updated := 0
	for _, seed := range seeds {
		row := byKey[seed.Key]
		if row == nil || !row.System || row.Kind != "builtin" {
			continue
		}
		var edited map[string]any
		// A legacy/non-object JSON value has no supported annotations; repair
		// its structure instead of abandoning the remaining catalog checks.
		_ = json.Unmarshal(row.Schema, &edited)
		raw, err := json.Marshal(agent.ReconcileToolSchema(seed.Schema, edited))
		if err != nil {
			return err
		}
		changed, err := pg.RefreshToolSchema(seed.Key, row.Schema, raw)
		if err != nil {
			return err
		}
		if changed {
			updated++
		}
	}
	if updated > 0 {
		log.Printf("[tools] 已核对并补齐%d项内置工具契约，保留启停、绑定、延迟和参数文案/有效默认值", updated)
	}
	return nil
}
