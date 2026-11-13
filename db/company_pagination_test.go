package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type companyPageFixture struct {
	db     *DB
	prefix string
	ids    []int64
	scopes map[int64]int
	assets map[int64]int
}

func newCompanyPageFixture(t *testing.T, count int) companyPageFixture {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	f := companyPageFixture{db: d, prefix: fmt.Sprintf("page-%d-", time.Now().UnixNano()), scopes: map[int64]int{}, assets: map[int64]int{}}
	t.Cleanup(func() {
		d.Exec(`DELETE FROM assets WHERE company_id=ANY($1)`, f.ids)
		d.Exec(`DELETE FROM company_scope WHERE company_id=ANY($1)`, f.ids)
		d.Exec(`DELETE FROM companies WHERE id=ANY($1)`, f.ids)
	})
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("%sCompany %02d", f.prefix, i)
		if i == count-1 || i == count-2 {
			name = f.prefix + "Shared" // Legacy rows may have equal display names.
		}
		var id int64
		if err := tx.QueryRow(`INSERT INTO companies(name,nkey) VALUES($1,$2) RETURNING id`, name, fmt.Sprintf("%skey-%d", f.prefix, i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		f.ids = append(f.ids, id)
		rules := 2
		if i == 0 {
			rules = 0
		} else if i == 1 {
			rules = 17
		}
		f.scopes[id] = rules
		for j := 0; j < rules; j++ {
			domain := fmt.Sprintf("%s%d-%d.fixture.invalid", f.prefix, i, j)
			if _, err := tx.Exec(`INSERT INTO company_scope(company_id,kind,domain,raw) VALUES($1,'domain',$2,$2)`, id, domain); err != nil {
				t.Fatal(err)
			}
		}
		f.assets[id] = i % 4
		for j := 0; j < i%4; j++ {
			domain := fmt.Sprintf("%sasset-%d-%d.fixture.invalid", f.prefix, i, j)
			if _, err := tx.Exec(`INSERT INTO assets(type,domain,company_id,company_source) VALUES('root_domain',$1,$2,'explicit')`, domain, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestCompanyPaginationExactTotalsStableOrderAndCompleteScopes(t *testing.T) {
	f := newCompanyPageFixture(t, 53)
	cs := f.db.Companies()
	seen := map[int64]bool{}
	previousName := ""
	var previousID int64
	for page := 1; page <= 8; page++ {
		result, err := cs.ListCompaniesPage(t.Context(), f.prefix, page, 7)
		if err != nil || result.Total != 53 || result.Page != page || result.PageSize != 7 {
			t.Fatalf("page=%d result=%+v err=%v", page, result, err)
		}
		for _, item := range result.Items {
			if seen[item.ID] {
				t.Fatalf("repeated company %d", item.ID)
			}
			seen[item.ID] = true
			if item.Name < previousName || (item.Name == previousName && item.ID <= previousID) {
				t.Fatalf("unstable name/id order: previous=%q/%d item=%+v", previousName, previousID, item)
			}
			previousName, previousID = item.Name, item.ID
			if len(item.Scope) != f.scopes[item.ID] || item.AssetCount != f.assets[item.ID] || item.Scope == nil {
				t.Fatalf("scope/asset counts were truncated or multiplied: %+v", item)
			}
			for _, rule := range item.Scope {
				if rule.CompanyID != item.ID {
					t.Fatalf("another company's scope leaked: %+v", rule)
				}
			}
		}
	}
	if len(seen) != 53 {
		t.Fatalf("missing companies: %d", len(seen))
	}
	beyond, err := cs.ListCompaniesPage(t.Context(), f.prefix, 100, 7)
	if err != nil || beyond.Total != 53 || len(beyond.Items) != 0 || beyond.Items == nil {
		t.Fatalf("empty page lost total: %+v err=%v", beyond, err)
	}
	filtered, err := cs.ListCompaniesPage(t.Context(), f.prefix+"shared", 1, 20)
	if err != nil || filtered.Total != 2 || len(filtered.Items) != 2 {
		t.Fatalf("case-insensitive exact total: %+v err=%v", filtered, err)
	}
	literal, err := cs.ListCompaniesPage(t.Context(), f.prefix+"%", 1, 20)
	if err != nil || literal.Total != 0 {
		t.Fatalf("search must not expand wildcard scope: %+v err=%v", literal, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cs.ListCompaniesPage(ctx, f.prefix, 1, 7); err == nil {
		t.Fatal("cancelled request still completed")
	}
}

type countedCompanyScopeReader struct {
	*DB
	queries int
}

func (r *countedCompanyScopeReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	r.queries++
	return r.DB.QueryContext(ctx, query, args...)
}

func TestCompanyBatchScopesAndLegacyFullList(t *testing.T) {
	f := newCompanyPageFixture(t, 31)
	cs := f.db.Companies()
	full, err := cs.ListCompanies()
	if err != nil {
		t.Fatal(err)
	}
	matched := make([]*CompanyWithScope, 0)
	for _, item := range full {
		if !strings.HasPrefix(item.Name, f.prefix) {
			continue
		}
		matched = append(matched, item)
		if len(item.Scope) != f.scopes[item.ID] || item.AssetCount != f.assets[item.ID] {
			t.Fatalf("legacy full-list contract changed: %+v", item)
		}
	}
	if len(matched) != 31 {
		t.Fatalf("legacy list accidentally paginated: %d", len(matched))
	}
	reader := &countedCompanyScopeReader{DB: f.db}
	if err := fillCompanyScopesContext(t.Context(), reader, matched); err != nil {
		t.Fatal(err)
	}
	if reader.queries != 1 {
		t.Fatalf("scope batching ran %d real SQL queries for %d companies", reader.queries, len(matched))
	}
	names, err := cs.ListCompanyNames(t.Context(), f.prefix)
	if err != nil || len(names) != 31 {
		t.Fatalf("name-only list truncated: %+v err=%v", names, err)
	}
	raw, err := json.Marshal(names)
	if err != nil || strings.Contains(string(raw), "scope") || strings.Contains(string(raw), "asset_count") {
		t.Fatalf("name-only response contains heavy payload: %s err=%v", raw, err)
	}
	var plan []struct {
		Plan struct {
			ActualRows int `json:"Actual Rows"`
		} `json:"Plan"`
		PlanningTime  float64 `json:"Planning Time"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	var rawPlan []byte
	if err := f.db.QueryRowContext(t.Context(), `EXPLAIN (ANALYZE, FORMAT JSON) `+companyPageSQL, f.prefix, 7, 0).Scan(&rawPlan); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawPlan, &plan); err != nil || len(plan) != 1 || plan[0].Plan.ActualRows != 7 {
		t.Fatalf("page query plan=%s err=%v", rawPlan, err)
	}
	t.Logf("合成31企业：scope批取实测1条SQL；页查询返回%d行，planning=%.3fms execution=%.3fms（仅合成数据，不外推加速比）", plan[0].Plan.ActualRows, plan[0].PlanningTime, plan[0].ExecutionTime)
}
