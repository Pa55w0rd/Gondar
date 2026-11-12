package agent

import "gondar/db"

func compactAssets(assets []*db.Asset) []map[string]any {
	out := make([]map[string]any, 0, len(assets))
	for _, a := range assets {
		tech := a.Technologies
		if len(tech) > 12 {
			tech = tech[:12]
		}
		out = append(out, map[string]any{"id": a.ID, "type": a.Type, "domain": a.Domain, "ip": a.IP,
			"url": a.URL, "port": a.Port, "method": a.Method, "status_code": a.StatusCode,
			"page_title": firstLine(a.PageTitle, 160), "technologies": tech, "auth_available": len(a.Auth) > 0,
			"task_source": a.TaskSource, "task_source_node_id": a.TaskSourceNodeID})
	}
	return out
}
