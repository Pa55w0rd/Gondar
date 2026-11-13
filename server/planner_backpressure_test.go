package server

import (
	"gondar/agent"
	"gondar/db"
	"testing"
)

func TestBackloggedHeartbeatKeepsConcreteEventsAndIdleFrontiers(t *testing.T) {
	full := db.IntentBacklog{Open: 32, Running: 1}
	if !skipBackloggedHeartbeat("heartbeat", nil, full) {
		t.Fatal("productive backlog not coalesced")
	}
	for _, source := range []string{"edge", "bounded-retry", "user"} {
		if skipBackloggedHeartbeat(source, nil, full) {
			t.Fatalf("lost %s", source)
		}
	}
	if skipBackloggedHeartbeat("heartbeat", []agent.TriggerEvent{{}}, full) {
		t.Fatal("lost concrete change")
	}
	if skipBackloggedHeartbeat("heartbeat", nil, db.IntentBacklog{Open: 32}) {
		t.Fatal("queue with no actual running work became silent")
	}
	if skipBackloggedHeartbeat("heartbeat", nil, db.IntentBacklog{Running: 1}) {
		t.Fatal("empty frontier not replenished")
	}
}
