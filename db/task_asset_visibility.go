package db

import (
	"fmt"
	"strings"
)

const (
	AssetContextDeclaredScope = "declared_scope"
	AssetContextTaskSources   = "task_sources"
)

func NormalizeAssetContextMode(mode string) (string, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return AssetContextDeclaredScope, nil
	}
	if mode != AssetContextDeclaredScope && mode != AssetContextTaskSources {
		return "", fmt.Errorf("资料来源模式必须是 declared_scope 或 task_sources")
	}
	return mode, nil
}

func normalizeArchivedAssetContextMode(row map[string]any) error {
	value := row["asset_context_mode"]
	if value == nil {
		row["asset_context_mode"] = AssetContextDeclaredScope
		return nil
	}
	raw, ok := value.(string)
	if !ok {
		return fmt.Errorf("%w: invalid asset_context_mode", ErrTaskArchiveFormatMismatch)
	}
	mode, err := NormalizeAssetContextMode(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTaskArchiveFormatMismatch, err)
	}
	row["asset_context_mode"] = mode
	return nil
}

func (s *AssetStore) TaskAssetContextMode(taskID int64) (string, error) {
	if taskID <= 0 {
		return AssetContextDeclaredScope, nil
	}
	var mode string
	if err := s.db.QueryRow(`SELECT asset_context_mode FROM tasks WHERE id=$1 AND deleted_at IS NULL`, taskID).Scan(&mode); err != nil {
		return "", err
	}
	return NormalizeAssetContextMode(mode)
}

// GetWorkerAssets preserves legacy bootstrap behavior, while the explicit
// narrow mode must filter before a caller can derive any automatic scope.
func (s *AssetStore) GetWorkerAssets(taskID int64, ids []int64) ([]*Asset, error) {
	mode, err := s.TaskAssetContextMode(taskID)
	if err != nil {
		return nil, err
	}
	if mode == AssetContextTaskSources {
		return s.GetByIDsInScope(taskID, ids)
	}
	return s.GetByIDs(ids)
}

func (s *AssetStore) CountsByTypeInScope(taskID int64) (map[string]int, error) {
	rows, err := s.db.Query(`WITH `+scopeTargetCTE+` SELECT a.type,count(*) FROM assets a JOIN target t ON t.id=a.id GROUP BY a.type`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var typ string
		var count int
		if err := rows.Scan(&typ, &count); err != nil {
			return nil, err
		}
		out[typ] = count
	}
	return out, rows.Err()
}

// ListCompaniesInTask is a material view, not a grant to execute company scope.
// Counts use the same source-and-scope intersection as list_assets. Explicitly
// selected empty companies remain visible so operators can populate them.
func (s *CompanyStore) ListCompaniesInTask(taskID int64) ([]*CompanyWithScope, error) {
	rows, err := s.db.Query(`WITH `+scopeTargetCTE+`, counts AS (
 SELECT a.company_id,count(*) AS n FROM assets a JOIN target t ON t.id=a.id WHERE a.company_id IS NOT NULL GROUP BY a.company_id
)
SELECT c.id,c.name,c.nkey,c.logo,c.created_at::text,c.updated_at::text,COALESCE(counts.n,0)
FROM companies c LEFT JOIN counts ON counts.company_id=c.id
WHERE counts.n IS NOT NULL OR EXISTS(SELECT 1 FROM task_scope ts JOIN context_tasks ctx ON ctx.task_id=ts.task_id WHERE ts.kind='company' AND ts.company_id=c.id)
ORDER BY c.name,c.id`, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]*CompanyWithScope, 0)
	for rows.Next() {
		item := &CompanyWithScope{}
		if err := rows.Scan(&item.ID, &item.Name, &item.NKey, &item.Logo, &item.CreatedAt, &item.UpdatedAt, &item.AssetCount); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, item)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return out, fillCompanyScopes(s.db, out)
}
