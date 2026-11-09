package db

// FindingReportGap describes empty report bodies, not the validity or severity
// of the finding itself. SQL counts every match while returning at most five IDs.
type FindingReportGap struct {
	Total     int64
	SampleIDs []int64
}

func (d *DB) FindingReportGaps(taskID int64) (FindingReportGap, error) {
	gap := FindingReportGap{SampleIDs: []int64{}}
	rows, err := d.Query(`SELECT count(*) OVER(),id FROM findings
WHERE task_id=$1 AND btrim(COALESCE(report,''), E' \t\n\r')=''
ORDER BY id LIMIT 5`, taskID)
	if err != nil {
		return gap, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&gap.Total, &id); err != nil {
			return gap, err
		}
		gap.SampleIDs = append(gap.SampleIDs, id)
	}
	return gap, rows.Err()
}
