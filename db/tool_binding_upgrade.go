package db

// UpgradeWorkerFactReader repairs the former default assembly mismatch once.
// Custom bindings and globally disabled tools retain the user's configuration.
func (d *DB) UpgradeWorkerFactReader() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(704030010)`); err != nil {
		return err
	}
	var done bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM settings WHERE key='worker_fact_reader_v030')`).Scan(&done); err != nil {
		return err
	}
	if done {
		return nil
	}
	if _, err := tx.Exec(`UPDATE tools SET agents=agents||'"worker"'::jsonb WHERE key='list_facts' AND system AND enabled AND jsonb_array_length(agents)=2 AND agents ? 'mainagent' AND agents ? 'planner'`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES('worker_fact_reader_v030','true') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}
