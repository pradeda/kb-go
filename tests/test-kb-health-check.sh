#!/usr/bin/env bash
set -euo pipefail

ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT
PROJECT_ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# The repo copy is the source of truth; the deployed /home/turok/scripts copy
# is produced from it by `make install` (see DEPLOY_MANIFEST).
WRAPPER="$PROJECT_ROOT/runtime/kb-health-check.sh"

cat >"$ROOT/compile.py" <<'PY'
import os
import sys

# Fixture switch for the compile.py --health contract (see kb-health-check.sh):
#   ok       -> exit 0
#   busy     -> exit 3 (mutating run held the corpus lock for the whole budget)
#   mismatch -> exit 1 with a real invariant failure
mode = os.environ.get("KB_HEALTH_FIXTURE_COMPILE", "ok")
if "--health" in sys.argv:
    if mode == "busy":
        print('{"corpus": "homelab", "lock": "/tmp/kb-watcher.lock", "status": "busy"}')
        raise SystemExit(3)
    if mode == "mismatch":
        print("RuntimeError: homelab SQLite/Chroma ID mismatch: missing=[] orphans=['1']")
        raise SystemExit(1)
raise SystemExit(0)
PY

cat >"$ROOT/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
for arg in "$@"; do
    if [[ "$arg" == https://api.telegram.org/* ]]; then
        printf 'notified\n' >>"$KB_HEALTH_NOTIFY_LOG"
        exit 0
    fi
done
printf '%s\n' "$KB_HEALTH_FIXTURE"
SH
chmod +x "$ROOT/curl"

cat >"$ROOT/python" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [ "${KB_HEALTH_FAIL_SEARCH_IMPORT:-0}" = 1 ] &&
   [ "${1:-}" = -c ] && [[ "${2:-}" == *sentence_transformers* ]]; then
    printf '%s\n' 'forced search import failure' >&2
    exit 42
fi
if [ "${KB_HEALTH_FAIL_EMBED_IMPORT:-0}" = 1 ] &&
   [ "${1:-}" = -c ] && [[ "${2:-}" == *chromadb* ]]; then
    printf '%s\n' 'forced embed import failure' >&2
    exit 43
fi
exec /usr/bin/python3 "$@"
SH
chmod +x "$ROOT/python"

cat >"$ROOT/telegram.env" <<'ENV'
BOT_TOKEN=test-token
CHAT_ID=test-chat
ENV

export KB_HEALTH_COMPILE="$ROOT/compile.py"
export KB_HEALTH_CURL="$ROOT/curl"
export KB_HEALTH_ENV="$ROOT/telegram.env"
export KB_HEALTH_NOTIFY_LOG="$ROOT/notifications"
export KB_HEALTH_PY_EMBED="$ROOT/python"
export KB_HEALTH_PY_SEARCH="$ROOT/python"

export KB_HEALTH_FIXTURE='{"status":"ok","rerank_model":"test-model"}'
"$WRAPPER" >/dev/null
test ! -e "$KB_HEALTH_NOTIFY_LOG"

export KB_HEALTH_FIXTURE='{"status":"degraded","rerank_model":null}'
if "$WRAPPER" >"$ROOT/stdout" 2>"$ROOT/stderr"; then
    echo "expected missing reranker to fail" >&2
    exit 1
fi
grep -q 'rerank_model' "$ROOT/stderr"
test "$(wc -l <"$KB_HEALTH_NOTIFY_LOG")" -eq 1

rm "$KB_HEALTH_NOTIFY_LOG"
export KB_HEALTH_FIXTURE='{"status":"ok","rerank_model":"test-model"}'
export KB_HEALTH_FAIL_SEARCH_IMPORT=1
if "$WRAPPER" >"$ROOT/stdout" 2>"$ROOT/stderr"; then
    echo "expected search import failure to fail" >&2
    exit 1
fi
grep -q '\[search-python\] import: forced search import failure' "$ROOT/stderr"
test "$(wc -l <"$KB_HEALTH_NOTIFY_LOG")" -eq 1

rm "$KB_HEALTH_NOTIFY_LOG"
unset KB_HEALTH_FAIL_SEARCH_IMPORT
export KB_HEALTH_FAIL_EMBED_IMPORT=1
if "$WRAPPER" >"$ROOT/stdout" 2>"$ROOT/stderr"; then
    echo "expected embed import failure to fail" >&2
    exit 1
fi
grep -q '\[embed-python\] import: forced embed import failure' "$ROOT/stderr"
test "$(wc -l <"$KB_HEALTH_NOTIFY_LOG")" -eq 1

rm "$KB_HEALTH_NOTIFY_LOG"
unset KB_HEALTH_FAIL_EMBED_IMPORT

# exit 3 from --health is "busy", not a fault: the run must succeed and must
# NOT notify. Distinguished by exit code only — never by grepping the output.
export KB_HEALTH_FIXTURE_COMPILE=busy
if ! "$WRAPPER" >"$ROOT/stdout" 2>"$ROOT/stderr"; then
    echo "busy health must not fail the check" >&2
    exit 1
fi
grep -q 'busy (mutacija u toku)' "$ROOT/stdout"
test ! -e "$KB_HEALTH_NOTIFY_LOG"

# A real invariant failure still fails and still alarms.
export KB_HEALTH_FIXTURE_COMPILE=mismatch
if "$WRAPPER" >"$ROOT/stdout" 2>"$ROOT/stderr"; then
    echo "expected health mismatch to fail" >&2
    exit 1
fi
grep -q 'orphans' "$ROOT/stderr"
grep -q 'health (rc=1)' "$ROOT/stderr"
test "$(wc -l <"$KB_HEALTH_NOTIFY_LOG")" -eq 1

rm "$KB_HEALTH_NOTIFY_LOG"
unset KB_HEALTH_FIXTURE_COMPILE

export KB_HEALTH_PY="$ROOT/python"
if "$WRAPPER" >"$ROOT/stdout" 2>"$ROOT/stderr"; then
    echo "expected legacy interpreter override to fail" >&2
    exit 1
fi
grep -q 'KB_HEALTH_PY je uklonjen' "$ROOT/stderr"
test "$(wc -l <"$KB_HEALTH_NOTIFY_LOG")" -eq 1

echo "kb-health-check import and reranker tests: PASS"
