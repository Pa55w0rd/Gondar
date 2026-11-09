package db

// MarkTaskExecuting records actual agent execution, not merely a started idle
// loop. The predicate is the lifecycle boundary: never revive paused, queued,
// deleted or terminal tasks from a stale in-memory created status.
func (d *DB) MarkTaskExecuting(id int64) (bool, error) {
	res, err := d.Exec(`UPDATE tasks SET status='running' WHERE id=$1 AND status='created' AND NOT paused AND NOT queued AND deleted_at IS NULL`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
