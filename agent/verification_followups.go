package agent

import "gondar/db"

// This is a bounded read view of existing records, never new goals, authority,
// execution evidence, or proof that unrecorded conditions have been covered.
func verificationFollowups(page *db.VerificationPage) []map[string]any {
	out := []map[string]any{}
	if page == nil {
		return out
	}
	for _, record := range page.Cases {
		if record == nil || record.State == db.VerificationConfirmed || record.State == db.VerificationNegative {
			continue
		}
		roles := map[string]bool{}
		for _, evidence := range record.Evidence {
			roles[evidence.Role] = true
		}
		missing := []string{}
		for _, role := range []string{"baseline", "test", "control", "impact"} {
			if !roles[role] {
				missing = append(missing, role)
			}
		}
		out = append(out, map[string]any{
			"verification_id": record.ID, "revision": record.Revision, "node_id": record.NodeID, "intent_id": record.IntentID,
			"target": record.Target, "state": record.State, "missing_evidence_roles": missing,
			"prerequisites_met": record.PrerequisitesMet, "missing_prerequisites": record.MissingPrerequisites, "blocker": record.Blocker,
			"next_step": "先读取当前修订及已有回执，核对真实消费者/缺口和原范围；只派发前提齐全且未重复的未知边。缺少impact不要求所有阴性格补造影响。",
		})
		if len(out) == 6 {
			break
		}
	}
	return out
}
