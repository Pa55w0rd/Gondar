package db

// FindingCounts is the exact aggregate of the same live rows as the paginated
// finding list. Archived task summaries remain in FindingStats' global totals;
// they cannot be filtered as if their original rows were still in this table.
type FindingCounts struct {
	Total    int `json:"total"`
	Pending  int `json:"pending"`
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
}

func (d *DB) FilteredFindingCounts(f FindingFilter) (*FindingCounts, error) {
	f, err := d.applyAssetScope(f)
	if err != nil {
		return nil, err
	}
	where, args := f.where()
	counts := &FindingCounts{}
	err = d.QueryRow(`SELECT COUNT(*),
		COUNT(*) FILTER (WHERE COALESCE(f.status,'pending')='pending'),
		COUNT(*) FILTER (WHERE f.severity='critical'),
		COUNT(*) FILTER (WHERE f.severity='high'),
		COUNT(*) FILTER (WHERE f.severity='medium'),
		COUNT(*) FILTER (WHERE f.severity='low')
		FROM findings f LEFT JOIN tasks t ON f.task_id=t.id`+where, args...).Scan(
		&counts.Total, &counts.Pending, &counts.Critical, &counts.High, &counts.Medium, &counts.Low)
	return counts, err
}
