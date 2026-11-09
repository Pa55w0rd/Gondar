package doctor

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDoctorPostgresOnlyReadsAndDoesNotBootstrapSchema(t *testing.T) {
	dsn := os.Getenv("GONDAR_PG_DSN")
	if dsn == "" {
		t.Skip("需要独立测试 PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	schema := "doctor_readonly_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	if check := inspectPostgres(ctx, u.String()); check.Status != "ok" {
		t.Fatalf("read-only connection: %#v", check)
	}
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM information_schema.tables WHERE table_schema=$1", schema).Scan(&count); err != nil || count != 0 {
		t.Fatalf("doctor bootstrapped application tables: count=%d error=%v", count, err)
	}
}
