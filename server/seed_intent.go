package server

import (
	"gondar/agent"
)

// The optional fast path is for a short, single direction. A full scenario is
// kept in the task root and handed to the planner instead of being truncated
// into one worker intent that conflates the task goal with an executable step.
func directSeedSummary(t *Task) (string, bool) {
	return agent.SeedIntentSummary(t.Description, t.Goal)
}
