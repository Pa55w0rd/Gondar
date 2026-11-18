package db

import "testing"

// Exercise the builder's composition contract with real PostgreSQL precedence,
// without relying on a specific caller to add another pair of parentheses.
func TestDSLBuilderOrComposesWithTaskAndTypeFilters(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	node, err := ParseDSL("url=profile OR url=token")
	if err != nil {
		t.Fatal(err)
	}
	where, values, err := buildDSLWhereBase(node, 1)
	if err != nil {
		t.Fatal(err)
	}
	args := append([]any{int64(1)}, values...)
	var count int
	err = d.QueryRow(`WITH fixture(url,type,task_ids) AS (VALUES
('https://one.invalid/profile','endpoint',ARRAY[1]::bigint[]),
('https://one.invalid/token','endpoint',ARRAY[1]::bigint[]),
('https://two.invalid/profile','endpoint',ARRAY[2]::bigint[]),
('https://one.invalid/profile','service',ARRAY[1]::bigint[]))
SELECT count(*) FROM fixture WHERE `+where+` AND $1=ANY(task_ids) AND type='endpoint'`, args...).Scan(&count)
	if err != nil || count != 2 {
		t.Fatalf("composed OR returned %d rows, want 2: %v", count, err)
	}
}
