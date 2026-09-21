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

# A refreshable entry with a stubbed fetch. The UPDATE must run, and it must run
# under the corpus lock: the lock file appears only if flock opened it.
cat > "$ROOT/curl-stub" <<'SH'
#!/usr/bin/env bash
printf '{"data":"Title: Fresh title\\n\\nfresh body"}'
SH
chmod +x "$ROOT/curl-stub"
printf 'pass\n' > "$ROOT/compile-noop.py"
LOCK="$ROOT/kb-watcher.lock"
/usr/bin/sqlite3 "$DB" "UPDATE entries SET content='https://example.invalid/x' WHERE id=58;"
KB_REFRESH_DB="$DB" KB_REFRESH_ENV="$ROOT/missing.env" \
    KB_REFRESH_CURL="$ROOT/curl-stub" KB_REFRESH_PYTHON=/usr/bin/python3 \
    KB_REFRESH_LOCK="$LOCK" KB_REFRESH_COMPILE="$ROOT/compile-noop.py" \
    "$REFRESH" >"$ROOT/out" 2>&1
grep -q 'OK: Fresh title' "$ROOT/out"
[ -e "$LOCK" ] || { echo 'refresh did not take the corpus lock' >&2; exit 1; }
row=$(/usr/bin/sqlite3 -noheader "$DB" \
    "SELECT title || '|' || coalesce(embedded_at,'NULL') || '|' || (content LIKE '%fresh body%') FROM entries WHERE id=58;")
[ "$row" = 'Fresh title|NULL|1' ] || {
    echo "unexpected refreshed row: $row" >&2; exit 1; }
/usr/bin/sqlite3 "$DB" "UPDATE entries SET content='multiline fetched content', title='Legacy', embedded_at=NULL WHERE id=58;"

cat > "$ROOT/sqlite-fail" <<'SH'
#!/usr/bin/env bash
echo 'forced sqlite failure' >&2
exit 17
SH
chmod +x "$ROOT/sqlite-fail"
# The flock wrapper must not swallow the wrapped command's exit status.
if KB_REFRESH_SQLITE="$ROOT/sqlite-fail" KB_REFRESH_ENV="$ROOT/missing.env" \
    KB_REFRESH_CURL="$ROOT/curl-stub" KB_REFRESH_PYTHON=/usr/bin/python3 \
    KB_REFRESH_LOCK="$LOCK" KB_REFRESH_COMPILE="$ROOT/compile-noop.py" \
    "$REFRESH" >"$ROOT/out" 2>"$ROOT/err"; then
    echo 'expected sqlite failure' >&2
    exit 1
fi
grep -q 'forced sqlite failure' "$ROOT/err"

echo 'refresh_volatile tests: PASS'
