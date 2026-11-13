package server

import (
	"encoding/json"
	"gondar/agent"
	"gondar/db"
	"log"
)

func refreshExecutionToolContracts(pg *db.DB) error {
	const flag = "execution_tool_contracts_v033"
	if value, _, _ := pg.GetSetting(flag); value == "true" {
		return nil
	}
	for _, seed := range agent.BuiltinToolSeeds() {
		switch seed.Key {
		case "record_verification", "list_verifications", "add_intent", "list_assets", "list_findings", "record_planner_wait", "add_exploration_goal":
		default:
			continue
		}
		raw, err := json.Marshal(seed.Schema)
		if err != nil {
			return err
		}
		// Existing bindings, enable/defer flags and custom tools are preserved.
		if err := pg.RefreshToolDefaults(seed.Key, seed.Desc, raw); err != nil {
			return err
		}
	}
	if err := pg.SetSetting(flag, "true"); err != nil {
		return err
	}
	log.Printf("[tools] 已同步执行、验证与分页工具契约，保留启停及角色绑定")
	return nil
}
