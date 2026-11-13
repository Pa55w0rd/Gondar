package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gondar/db"
)

func TestCompanyListAPIKeepsFullScopeAndOptInPagination(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skip("企业分页集成回归需要专用 PostgreSQL")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	prefix := fmt.Sprintf("api-page-%d-", time.Now().UnixNano())
	ids := make([]int64, 0, 23)
	defer func() {
		pg.Exec(`DELETE FROM company_scope WHERE company_id=ANY($1)`, ids)
		pg.Exec(`DELETE FROM companies WHERE id=ANY($1)`, ids)
	}()
	for i := 0; i < 23; i++ {
		name := fmt.Sprintf("%s%02d", prefix, i)
		var id int64
		if err := pg.QueryRow(`INSERT INTO companies(name,nkey) VALUES($1,$1) RETURNING id`, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		if i == 22 {
			for j := 0; j < 17; j++ {
				domain := fmt.Sprintf("%s%d.fixture.invalid", prefix, j)
				if _, err := pg.Exec(`INSERT INTO company_scope(company_id,kind,domain,raw) VALUES($1,'domain',$2,$2)`, id, domain); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	s := &Server{m: &Manager{pg: pg}}
	request := func(query string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.listCompanies(w, httptest.NewRequest(http.MethodGet, "/api/companies"+query, nil))
		return w
	}
	legacy := request("")
	var all []db.CompanyWithScope
	if err := json.Unmarshal(legacy.Body.Bytes(), &all); err != nil || legacy.Code != http.StatusOK {
		t.Fatalf("legacy response is no longer an array: code=%d body=%s err=%v", legacy.Code, legacy.Body.String(), err)
	}
	matched := 0
	for _, item := range all {
		if strings.HasPrefix(item.Name, prefix) {
			matched++
		}
		if item.ID == ids[22] && len(item.Scope) != 17 {
			t.Fatalf("task creation's legacy list lost full scope: %+v", item)
		}
	}
	if matched != 23 {
		t.Fatalf("legacy endpoint was silently paginated: %d", matched)
	}
	paged := request("?page=5&page_size=5&q=" + url.QueryEscape(prefix))
	var page db.CompanyPage
	if err := json.Unmarshal(paged.Body.Bytes(), &page); err != nil || paged.Code != http.StatusOK || page.Total != 23 || len(page.Items) != 3 {
		t.Fatalf("page total/items mismatch: code=%d body=%s err=%v", paged.Code, paged.Body.String(), err)
	}
	if page.Items[2].ID != ids[22] || len(page.Items[2].Scope) != 17 {
		t.Fatalf("last page scope was truncated: %+v", page.Items)
	}
	names := request("?names_only=1&q=" + url.QueryEscape(prefix))
	var light []map[string]any
	if err := json.Unmarshal(names.Body.Bytes(), &light); err != nil || names.Code != http.StatusOK || len(light) != 23 {
		t.Fatalf("name-only response truncated: code=%d body=%s err=%v", names.Code, names.Body.String(), err)
	}
	for _, item := range light {
		if len(item) != 2 || item["id"] == nil || item["name"] == nil {
			t.Fatalf("name-only payload is not light: %+v", item)
		}
	}
	for _, query := range []string{
		"?page=0", "?page=", "?page_size=201", "?names_only=invalid", "?names_only=1&page=1",
		"?page=9223372036854775807&page_size=200",
	} {
		if got := request(query); got.Code != http.StatusBadRequest {
			t.Errorf("query=%q status=%d body=%s", query, got.Code, got.Body.String())
		}
	}
}
