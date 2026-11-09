package db

import "encoding/json"

// Restoring a package is a new execution epoch: never let a pre-archive round
// write over restored handoff data before the next planner wake-up.
func fenceArchivedPlannerState(raw json.RawMessage) (json.RawMessage, error) {
	rows, err := decodeArchiveRows(raw)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return json.RawMessage(`[]`), nil
	}
	for _, row := range rows {
		rev, _ := jsonInt64(row["revision"])
		row["revision"] = rev + 1
		if row["outcome"] == "running" {
			row["outcome"] = "canceled"
		}
	}
	return json.Marshal(rows)
}
