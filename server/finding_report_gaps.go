package server

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"gondar/db"
)

// Call only after the guarded terminal transition won. This is a delivery hint,
// not a reason to extend the budget, restart work, or assert a vulnerability.
func (e *Engine) warnFindingsWithoutReport(t *Task) {
	if t == nil || e.m == nil || e.m.pg == nil {
		return
	}
	id, err := strconv.ParseInt(t.ID, 10, 64)
	if err != nil || id <= 0 {
		return
	}
	gap, err := e.m.pg.FindingReportGaps(id)
	if err != nil {
		log.Printf("[delivery] task %s 读取报告缺口失败: %v", t.ID, err)
		return
	}
	if gap.Total == 0 {
		return
	}
	ids := make([]string, len(gap.SampleIDs))
	for i, id := range gap.SampleIDs {
		ids[i] = fmt.Sprintf("#%d", id)
	}
	e.emitActivity(t, db.Activity{Worker: "system", Kind: "text", Summary: fmt.Sprintf(
		"收尾交付提示：本任务有 %d 条漏洞记录缺少详细报告（最多列5条：%s）。请核对后补齐再导出；此提示不代表漏洞已确认，也不自动延长或重启任务。", gap.Total, strings.Join(ids, "、"))})
}
