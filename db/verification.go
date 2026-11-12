package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	VerificationNotTested           = "not_tested"
	VerificationBlocked             = "blocked"
	VerificationMissingPrerequisite = "missing_prerequisite"
	VerificationNegative            = "negative"
	VerificationConfirmed           = "confirmed"
	VerificationInconclusive        = "inconclusive"
)

var ErrVerificationRevisionConflict = errors.New("验证记录版本已变化，请读取最新修订后再提交")

// VerificationTarget identifies one case, never an entire vulnerability class.
// Keep wire representations and object/identity labels distinct; URL decoding or
// wildcard expansion here would incorrectly merge different test conditions.
type VerificationTarget struct {
	Endpoint  string `json:"endpoint"`
	Method    string `json:"method"`
	Input     string `json:"input"`
	Identity  string `json:"identity"`
	Technique string `json:"technique"`
	Variant   string `json:"variant"`
}

// VerificationEvidence references already persisted, task-local evidence. The
// observation is the small decision-relevant content, not a raw response dump.
type VerificationEvidence struct {
	Role        string `json:"role"`
	Observation string `json:"observation"`
	ActivityID  int64  `json:"activity_id,omitempty"`
	NodeID      int64  `json:"node_id,omitempty"`
	ArtifactID  int64  `json:"artifact_id,omitempty"`
}

type VerificationUpdate struct {
	Target               VerificationTarget     `json:"target"`
	State                string                 `json:"state"`
	Summary              string                 `json:"summary"`
	Observation          string                 `json:"observation"`
	Evidence             []VerificationEvidence `json:"evidence,omitempty"`
	PrerequisitesMet     bool                   `json:"prerequisites_met"`
	Blocker              string                 `json:"blocker,omitempty"`
	MissingPrerequisites []string               `json:"missing_prerequisites,omitempty"`
	IntentID             int64                  `json:"intent_id,omitempty"`
	ExpectedRevision     *int64                 `json:"expected_revision,omitempty"`
	ChangeReason         string                 `json:"change_reason,omitempty"`
	ObservedAt           string                 `json:"observed_at,omitempty"`
}

// VerificationRecord lives inside a regular fact payload. Revisions are
// append-only facts, so existing graph archiving preserves their evidence.
type VerificationRecord struct {
	SchemaVersion        int                    `json:"schema_version"`
	ID                   string                 `json:"id"`
	Revision             int64                  `json:"revision"`
	PreviousNodeID       int64                  `json:"previous_node_id,omitempty"`
	ContentDigest        string                 `json:"content_digest"`
	Target               VerificationTarget     `json:"target"`
	State                string                 `json:"state"`
	Summary              string                 `json:"summary"`
	Observation          string                 `json:"observation"`
	Evidence             []VerificationEvidence `json:"evidence,omitempty"`
	PrerequisitesMet     bool                   `json:"prerequisites_met"`
	Blocker              string                 `json:"blocker,omitempty"`
	MissingPrerequisites []string               `json:"missing_prerequisites,omitempty"`
	IntentID             int64                  `json:"intent_id,omitempty"`
	ChangeReason         string                 `json:"change_reason,omitempty"`
	ObservedAt           string                 `json:"observed_at,omitempty"`
	Progress             bool                   `json:"progress"`
	NodeID               int64                  `json:"node_id,omitempty"`
	NodeState            string                 `json:"node_state,omitempty"`
	CreatedAt            time.Time              `json:"created_at,omitzero"`
}

type VerificationWrite struct {
	Changed bool                `json:"changed"`
	Record  *VerificationRecord `json:"record"`
}

type VerificationFilter struct {
	ID             string
	State          string
	Endpoint       string
	Before         int64
	Limit          int
	IncludeHistory bool
}

type VerificationPage struct {
	Cases         []*VerificationRecord `json:"cases"`
	Total         int                   `json:"total"`
	RecordedCases int                   `json:"recorded_cases"`
	LatestStates  map[string]int        `json:"latest_states"`
	HasMore       bool                  `json:"has_more"`
	NextBefore    int64                 `json:"next_before,omitempty"`
	Scope         string                `json:"scope"`
}

func verificationText(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(utf8Clean(text), "\r\n", "\n"))
}

func validVerificationState(state string) bool {
	switch state {
	case VerificationNotTested, VerificationBlocked, VerificationMissingPrerequisite,
		VerificationNegative, VerificationConfirmed, VerificationInconclusive:
		return true
	}
	return false
}

func normalizeVerificationUpdate(in VerificationUpdate) (VerificationUpdate, string, string, error) {
	fields := []*string{&in.Target.Endpoint, &in.Target.Method, &in.Target.Input,
		&in.Target.Identity, &in.Target.Technique, &in.Target.Variant, &in.State,
		&in.Summary, &in.Observation, &in.Blocker, &in.ChangeReason}
	for _, field := range fields {
		*field = verificationText(*field)
	}
	in.Target.Method = strings.ToUpper(in.Target.Method)
	if in.Target.Variant == "" {
		in.Target.Variant = "default"
	}
	if in.Target.Endpoint == "" || in.Target.Method == "" || in.Target.Input == "" || in.Target.Identity == "" || in.Target.Technique == "" {
		return in, "", "", fmt.Errorf("target必须明确endpoint/method/input/identity/technique；无参数或匿名身份也须使用明确标签")
	}
	if len(in.Target.Endpoint) > 2048 || len(in.Target.Method) > 32 || len(in.Target.Input) > 256 || len(in.Target.Identity) > 256 || len(in.Target.Technique) > 256 || len(in.Target.Variant) > 256 {
		return in, "", "", fmt.Errorf("验证范围标签过长")
	}
	if !validVerificationState(in.State) || in.Summary == "" || len(in.Summary) > 512 || len(in.Observation) > 4096 || len(in.Blocker) > 2048 || len(in.ChangeReason) > 2048 {
		return in, "", "", fmt.Errorf("验证状态、摘要或观察字段无效或过长")
	}
	if in.IntentID < 0 || (in.ExpectedRevision != nil && *in.ExpectedRevision < 0) {
		return in, "", "", fmt.Errorf("意图与修订号不能为负数")
	}
	if in.State != VerificationNotTested && in.Observation == "" {
		return in, "", "", fmt.Errorf("已执行或受阻状态必须写明实际观察")
	}
	if len(in.Evidence) > 16 || len(in.MissingPrerequisites) > 16 {
		return in, "", "", fmt.Errorf("每次最多16项证据或前置条件")
	}
	in.Evidence = append([]VerificationEvidence(nil), in.Evidence...)
	missing := map[string]bool{}
	for _, value := range in.MissingPrerequisites {
		value = verificationText(value)
		if value == "" || len(value) > 256 {
			return in, "", "", fmt.Errorf("缺失前置条件必须为简短的非空说明")
		}
		missing[value] = true
	}
	in.MissingPrerequisites = nil
	for value := range missing {
		in.MissingPrerequisites = append(in.MissingPrerequisites, value)
	}
	sort.Strings(in.MissingPrerequisites)
	roles := map[string]bool{}
	for i := range in.Evidence {
		e := &in.Evidence[i]
		e.Role, e.Observation = verificationText(e.Role), verificationText(e.Observation)
		switch e.Role {
		case "baseline", "test", "control", "impact", "blocker":
		default:
			return in, "", "", fmt.Errorf("证据role必须是baseline/test/control/impact/blocker")
		}
		refs := 0
		for _, id := range []int64{e.ActivityID, e.NodeID, e.ArtifactID} {
			if id < 0 {
				return in, "", "", fmt.Errorf("证据编号不能为负数")
			}
			if id > 0 {
				refs++
			}
		}
		if refs != 1 || e.Observation == "" || len(e.Observation) > 1536 {
			return in, "", "", fmt.Errorf("每项证据需要一项已保存的activity_id/node_id/artifact_id及简短实际观察")
		}
		roles[e.Role] = true
	}
	if in.State == VerificationBlocked && (in.Blocker == "" || len(in.Evidence) == 0) {
		return in, "", "", fmt.Errorf("被防护阻断需要具体阻断原因和已保存的回执；原因不明用inconclusive")
	}
	if in.State == VerificationMissingPrerequisite && len(in.MissingPrerequisites) == 0 {
		return in, "", "", fmt.Errorf("缺前提状态需要missing_prerequisites")
	}
	if in.State == VerificationNegative || in.State == VerificationConfirmed {
		if !in.PrerequisitesMet || in.Blocker != "" || len(in.MissingPrerequisites) > 0 || !roles["baseline"] || !roles["test"] {
			return in, "", "", fmt.Errorf("阴性或证实需要前提已满足、无阻断、正常基线和实际测试证据")
		}
		if in.State == VerificationNegative && !roles["control"] {
			return in, "", "", fmt.Errorf("有效阴性还需要control证据，仅覆盖当前输入/身份/变体")
		}
		if in.State == VerificationConfirmed && !roles["impact"] {
			return in, "", "", fmt.Errorf("证实还需要实际影响impact证据；状态码差异不能独立证明漏洞")
		}
	}
	if in.ObservedAt != "" {
		observed, err := time.Parse(time.RFC3339Nano, in.ObservedAt)
		if err != nil {
			return in, "", "", fmt.Errorf("observed_at必须是RFC3339时间")
		}
		in.ObservedAt = observed.UTC().Format(time.RFC3339Nano)
	}
	identity, _ := json.Marshal(in.Target)
	idHash := sha256.Sum256(identity)
	id := "vfy_" + hex.EncodeToString(idHash[:])
	// Reference IDs, producer, display title and timestamps are provenance, not
	// new business information. Equivalent replays must not refresh progress.
	semanticEvidence := map[string]bool{}
	for _, evidence := range in.Evidence {
		raw, _ := json.Marshal([]string{evidence.Role, evidence.Observation})
		semanticEvidence[string(raw)] = true
	}
	keys := make([]string, 0, len(semanticEvidence))
	for key := range semanticEvidence {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	semantic, _ := json.Marshal(struct {
		Target                      VerificationTarget
		State, Observation, Blocker string
		PrerequisitesMet            bool
		Missing, Evidence           []string
	}{in.Target, in.State, in.Observation, in.Blocker, in.PrerequisitesMet, in.MissingPrerequisites, keys})
	digest := sha256.Sum256(semantic)
	return in, id, hex.EncodeToString(digest[:]), nil
}
