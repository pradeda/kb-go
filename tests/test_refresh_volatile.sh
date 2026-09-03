#!/usr/bin/env bash
set -euo pipefail

ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT
PROJECT_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
REFRESH="$PROJECT_ROOT/runtime/refresh_volatile.sh"
DB="$ROOT/kb.db"
/usr/bin/sqlite3 "$DB" <<'SQL'
CREATE TABLE entries (
  id INTEGER PRIMARY KEY, type TEXT, content TEXT, title TEXT, summary TEXT,
  tags TEXT, compiled_at TEXT, embedded_at TEXT, created_at TEXT
);
INSERT INTO entries VALUES
  (58,'url','multiline fetched content','Legacy','','volatile',NULL,NULL,'2026-01-01');
SQL

output=$(KB_REFRESH_DB="$DB" KB_REFRESH_ENV="$ROOT/missing.env" \
    "$REFRESH" 2>&1)
grep -q 'skipped IDs: 58' <<< "$output"
grep -q 'No volatile URLs to refresh' <<< "$output"

cat > "$ROOT/sqlite-fail" <<'SH'
#!/usr/bin/env bash
echo 'forced sqlite failure' >&2
exit 17
SH
chmod +x "$ROOT/sqlite-fail"
if KB_REFRESH_SQLITE="$ROOT/sqlite-fail" KB_REFRESH_ENV="$ROOT/missing.env" \
    "$REFRESH" >"$ROOT/out" 2>"$ROOT/err"; then
    echo 'expected sqlite failure' >&2
    exit 1
fi
grep -q 'forced sqlite failure' "$ROOT/err"

echo 'refresh_volatile tests: PASS'
