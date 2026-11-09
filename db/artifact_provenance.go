package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

var ErrArtifactAlreadyBound = errors.New("附件已明确归属其他漏洞，请核对后由人工修改；自动补绑不会覆盖现有关联")

func (d *DB) CheckTaskArtifactOwner(ctx context.Context, a *TaskArtifact) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return lockArtifactOwner(tx, a)
}

// Task lock first, then intent/finding/artifact. Neither a path fragment nor a
// related task grants ownership of a newly-created artifact.
func lockArtifactOwner(tx *sql.Tx, a *TaskArtifact) error {
	tid, err := strconv.ParseInt(a.TaskID, 10, 64)
	if err != nil || tid <= 0 || a.IntentID < 0 || a.FindingID < 0 {
		return errors.New("附件任务、意图或漏洞编号无效")
	}
	if err := lockTaskExtension(tx, tid, 0); err != nil {
		return err
	}
	if a.IntentID > 0 {
		var owned bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM exploration_nodes n JOIN tasks t ON t.exploration_id=n.exploration_id WHERE t.id=$1 AND n.id=$2 AND n.kind='intent')`, tid, a.IntentID).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return errors.New("附件来源意图必须属于当前任务")
		}
	}
	if a.FindingID > 0 {
		var owned bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM findings WHERE id=$1 AND task_id=$2)`, a.FindingID, tid).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return errors.New("附件关联漏洞必须属于当前任务")
		}
	}
	return nil
}

func (d *DB) insertTaskArtifact(ctx context.Context, a *TaskArtifact) error {
	if a == nil {
		return errors.New("附件不能为空")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockArtifactOwner(tx, a); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO task_artifacts(task_id,finding_id,intent_id,name,kind,mime,size_bytes,sha256,source_url,note,storage_path,preview_state)
VALUES ($1,NULLIF($2,0),NULLIF($3,0),$4,$5,NULLIF($6,''),$7,NULLIF($8,''),NULLIF($9,''),NULLIF($10,''),$11,NULLIF($12,'')) RETURNING id,created_at`,
		a.TaskID, a.FindingID, a.IntentID, a.Name, a.Kind, a.Mime, a.SizeBytes, a.SHA256, a.SourceURL, a.Note, a.Storage, a.PreviewState).Scan(&a.ID, &a.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) updateArtifactAssociation(id, findingID int64, note, sourceURL string, manual bool) (*TaskArtifact, error) {
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var a TaskArtifact
	// This initial lookup is deliberately unlocked; global lock order is task,
	// then artifact. The UPDATE below rechecks the immutable task identity.
	if err := tx.QueryRow(`SELECT task_id FROM task_artifacts WHERE id=$1`, id).Scan(&a.TaskID); err != nil {
		return nil, err
	}
	a.FindingID = findingID
	if err := lockArtifactOwner(tx, &a); err != nil {
		return nil, err
	}
	query := `UPDATE task_artifacts SET finding_id=NULLIF($2,0)`
	args := []any{id, findingID, a.TaskID}
	if !manual {
		query += `,note=COALESCE(NULLIF(note,''),NULLIF($4,'')),source_url=COALESCE(NULLIF(source_url,''),NULLIF($5,''))`
		args = append(args, note, sourceURL)
	}
	query += ` WHERE id=$1 AND task_id=$3`
	if !manual {
		query += ` AND (finding_id IS NULL OR finding_id=NULLIF($2,0))`
	}
	query += ` RETURNING id`
	if err := tx.QueryRow(query, args...).Scan(&id); errors.Is(err, sql.ErrNoRows) && !manual {
		return nil, ErrArtifactAlreadyBound
	} else if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return d.GetTaskArtifact(id)
}

// Artifact metadata stays in its existing hot table while files/graph are cold.
// Intent IDs are historical references without a deletion FK. Restore keeps graph
// IDs stable (a conflicting node aborts restore); invalid references remain only
// history and are excluded by the same-task join used for association suggestions.
func validateArchivedArtifactIntents(tx *sql.Tx, taskID, expID int64) ([]string, error) {
	var present bool
	if err := tx.QueryRow(`SELECT to_regclass('task_artifacts') IS NOT NULL`).Scan(&present); err != nil || !present {
		return nil, err
	}
	var invalid int64
	if err := tx.QueryRow(`SELECT count(*) FROM task_artifacts a WHERE task_id=$1 AND intent_id>0 AND NOT EXISTS(SELECT 1 FROM exploration_nodes n WHERE n.id=a.intent_id AND n.exploration_id=$2 AND n.kind='intent')`, strconv.FormatInt(taskID, 10), expID).Scan(&invalid); err != nil {
		return nil, err
	}
	if invalid > 0 {
		return []string{fmt.Sprintf("%d份附件的历史意图来源无法在当前任务图核验，已排除同意图归属建议；原元数据仍保留", invalid)}, nil
	}
	return nil, nil
}

type ArtifactFindingSuggestion struct {
	FindingID int64  `json:"finding_id"`
	Name      string `json:"name"`
	Reason    string `json:"reason"`
}

// Read actual stored provenance; do not accept an intent ID from the request.
// Each branch rechecks current task ownership. Neither branch updates binding.
func (d *DB) ArtifactFindingSuggestions(taskID, artifactID int64) ([]ArtifactFindingSuggestion, bool, error) {
	items := []ArtifactFindingSuggestion{}
	rows, err := d.Query(`WITH current_artifact AS (
 SELECT a.* FROM task_artifacts a JOIN tasks t ON a.task_id=t.id::text
 WHERE t.id=$1 AND a.id=$2 AND t.deleted_at IS NULL AND t.archived_at IS NULL
), matches AS (
 SELECT f.id,COALESCE(NULLIF(f.name,''),f.vulnclass) AS name,true AS by_url,false AS by_intent
 FROM current_artifact ca JOIN findings f ON f.task_id=$1
 JOIN assets a ON f.asset_ids @> to_jsonb(ARRAY[a.id])
 WHERE COALESCE(ca.source_url,'')<>'' AND a.url=ca.source_url
 UNION ALL
 SELECT f.id,COALESCE(NULLIF(f.name,''),f.vulnclass),false,true
 FROM current_artifact ca JOIN tasks t ON t.id=$1
 JOIN exploration_nodes n ON n.id=ca.intent_id AND n.exploration_id=t.exploration_id AND n.kind='intent'
 JOIN exploration_edges e ON e.src_id=n.id AND e.exploration_id=t.exploration_id AND e.rel=$3
 JOIN findings f ON f.node_id=e.dst_id AND f.task_id=t.id
) SELECT id,max(name),bool_or(by_url),bool_or(by_intent) FROM matches GROUP BY id ORDER BY id DESC LIMIT 11`, taskID, artifactID, RelYields)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var v ArtifactFindingSuggestion
		var byURL, byIntent bool
		if err := rows.Scan(&v.FindingID, &v.Name, &byURL, &byIntent); err != nil {
			return nil, false, err
		}
		if len(items) == 10 {
			return items, true, nil
		}
		if byURL {
			v.Reason = "来源完整 URL 与该漏洞的受影响资产 URL 一致，仍需核对附件内容"
		}
		if byIntent {
			if v.Reason != "" {
				v.Reason += "；"
			}
			v.Reason += "材料与该漏洞来自同一执行意图，仅是出处线索，须核对内容后人工关联"
		}
		items = append(items, v)
	}
	return items, false, rows.Err()
}
