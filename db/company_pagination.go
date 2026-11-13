package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type CompanyName struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type CompanyPage struct {
	Items    []*CompanyWithScope `json:"items"`
	Total    int                 `json:"total"`
	Page     int                 `json:"page"`
	PageSize int                 `json:"page_size"`
}

// Select the page before counting its assets. Scope is fetched once for exactly
// these IDs; it remains complete, including for scope editing and task creation.
const companyPageSQL = `WITH page AS (
 SELECT id,name,nkey,logo,created_at,updated_at FROM companies
 WHERE $1='' OR strpos(lower(name),lower($1))>0
 ORDER BY name,id LIMIT $2 OFFSET $3
), asset_counts AS (
 SELECT company_id,count(*) AS asset_count FROM assets
 WHERE company_id IN (SELECT id FROM page) GROUP BY company_id
)
SELECT p.id,p.name,p.nkey,p.logo,p.created_at::text,p.updated_at::text,COALESCE(a.asset_count,0)
FROM page p LEFT JOIN asset_counts a ON a.company_id=p.id ORDER BY p.name,p.id`

func (s *CompanyStore) ListCompaniesPage(ctx context.Context, search string, page, size int) (CompanyPage, error) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = 20
	}
	if size > 200 {
		size = 200
	}
	out := CompanyPage{Items: make([]*CompanyWithScope, 0), Page: page, PageSize: size}
	if page-1 > int(^uint(0)>>1)/size {
		return out, errors.New("企业分页偏移过大")
	}
	// Count, page and scope all describe the same read snapshot even when a
	// concurrent request creates/deletes a company or edits its scope.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return out, err
	}
	defer tx.Rollback() //nolint:errcheck
	search = strings.TrimSpace(search)
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM companies WHERE $1='' OR strpos(lower(name),lower($1))>0`, search).Scan(&out.Total); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, companyPageSQL, search, size, int64(page-1)*int64(size))
	if err != nil {
		return out, err
	}
	for rows.Next() {
		item := &CompanyWithScope{}
		if err := rows.Scan(&item.ID, &item.Name, &item.NKey, &item.Logo, &item.CreatedAt, &item.UpdatedAt, &item.AssetCount); err != nil {
			rows.Close()
			return out, err
		}
		out.Items = append(out.Items, item)
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return out, err
	}
	if err := fillCompanyScopesContext(ctx, tx, out.Items); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

// ListCompanyNames intentionally returns no asset join or scope payload.
func (s *CompanyStore) ListCompanyNames(ctx context.Context, search string) ([]CompanyName, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name FROM companies WHERE $1='' OR strpos(lower(name),lower($1))>0 ORDER BY name,id`, strings.TrimSpace(search))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CompanyName, 0)
	for rows.Next() {
		var item CompanyName
		if err := rows.Scan(&item.ID, &item.Name); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

type companyScopeBatchReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func fillCompanyScopes(q companyScopeBatchReader, companies []*CompanyWithScope) error {
	return fillCompanyScopesContext(context.Background(), q, companies)
}

func fillCompanyScopesContext(ctx context.Context, q companyScopeBatchReader, companies []*CompanyWithScope) error {
	if len(companies) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(companies))
	byID := make(map[int64]*CompanyWithScope, len(companies))
	for _, item := range companies {
		ids = append(ids, item.ID)
		byID[item.ID] = item
		item.Scope = make([]ScopeRule, 0)
	}
	rows, err := q.QueryContext(ctx, `SELECT id,company_id,kind,COALESCE(domain,''),COALESCE(net::text,''),COALESCE(value,''),raw,COALESCE(reason,'')
FROM company_scope WHERE company_id=ANY($1) ORDER BY company_id,id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var rule ScopeRule
		if err := rows.Scan(&rule.ID, &rule.CompanyID, &rule.Kind, &rule.Domain, &rule.Net, &rule.Value, &rule.Raw, &rule.Reason); err != nil {
			return err
		}
		item := byID[rule.CompanyID]
		item.Scope = append(item.Scope, rule)
	}
	return rows.Err()
}
