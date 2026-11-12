#!/usr/bin/env bash
# Each test package receives a fresh database; no package inherits fixtures,
# notifications, model settings or tool overrides from a previous package.
set -euo pipefail
: "${GONDAR_TEST_PG_CONTAINER:?必须指定本轮专用PostgreSQL测试容器}"
: "${GONDAR_PG_DSN:?必须指定本轮测试连接}"
base_dsn="${GONDAR_PG_DSN%/*}"
test_db=""
cleanup() {
  if [[ "$test_db" =~ ^gondar_ci_[0-9]+_[0-9]+$ ]]; then
    docker exec "$GONDAR_TEST_PG_CONTAINER" psql -X -q -v ON_ERROR_STOP=1 -U gondar -d postgres -c "DROP DATABASE IF EXISTS $test_db WITH (FORCE)" >/dev/null
    test_db=""
  fi
}
trap cleanup EXIT
index=0
for package in $(go list ./...); do
  index=$((index+1))
  test_db="gondar_ci_$$_$index"
  docker exec "$GONDAR_TEST_PG_CONTAINER" psql -X -q -v ON_ERROR_STOP=1 -U gondar -d postgres -c "CREATE DATABASE $test_db" >/dev/null
  printf '正在测试独立数据库中的 %s\n' "$package" >&2
  GONDAR_PG_DSN="$base_dsn/$test_db?sslmode=disable" go test -p 1 "$package" -count=1 "$@"
  cleanup
done
