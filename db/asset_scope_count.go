package db

import "fmt"

// Match QueryDSLInScope's exact authorized asset set, including direct sources.
func (s *AssetStore) CountDSLInScope(dsl, typ string, taskID int64) (int, error) {
	if taskID <= 0 {
		return s.CountDSL(dsl, typ, 0)
	}
	node, err := ParseDSL(dsl)
	if err != nil {
		return 0, err
	}
	where, values, err := buildDSLWhereBase(node, 1)
	if err != nil {
		return 0, err
	}
	args := append([]any{taskID}, values...)
	where = "(" + where + ") AND id IN (SELECT id FROM target)"
	if typ != "" {
		args = append(args, typ)
		where += fmt.Sprintf(" AND type=$%d", len(args))
	}
	var total int
	err = s.db.QueryRow(`WITH `+scopeTargetCTE+` SELECT count(*) FROM assets WHERE `+where, args...).Scan(&total)
	return total, err
}
