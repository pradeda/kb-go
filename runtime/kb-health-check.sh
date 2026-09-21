#!/usr/bin/env bash
# Dnevna i post-boot provera oba KB corpusa.
#
# Redosled je namerno compile pa health: compile isprazni red neembedovanih
# unosa, pa health meri stvarno ustaljeno stanje umesto zaostatka koji bi se
# ionako razresio sekundu kasnije.
#
# Alarm se salje samo pri padu — zeleni prolaz ne javlja nista, inace se
# poruke prestanu citati.
#
# Ugovor sa compile.py --health:
#   exit 0  — invarijante proverene (JSON izlaz)
#   exit 3  — "busy": mutirajuci proces je drzao per-corpus lock ceo budzet;
#             stanje je nepoznato, NE alarmira se (vidi granu ispod)
#   ostalo  — stvarni kvar (mismatch, integrity, itd.) → alarm

set -uo pipefail

PY_EMBED="${KB_HEALTH_PY_EMBED:-/opt/kb/venv-embed/bin/python}"
PY_SEARCH="${KB_HEALTH_PY_SEARCH:-/opt/kb/venv-search/bin/python}"
COMPILE="${KB_HEALTH_COMPILE:-/opt/kb/compile.py}"
CURL="${KB_HEALTH_CURL:-/usr/bin/curl}"
TIMEOUT="${KB_HEALTH_TIMEOUT:-/usr/bin/timeout}"
SEARCH_HEALTH_URL="${KB_SEARCH_HEALTH_URL:-http://127.0.0.1:8050/health}"
ENV_FILE="${KB_HEALTH_ENV:-$HOME/.config/kb-health/telegram.env}"
CORPORA=(homelab ai)

# compile.py uzima per-corpus lock i ceka ako drugi pisac drzi pass, pa bez
# granice health provera moze da visi zauvek i da ne posalje nijedan alarm.
DRAIN_TIMEOUT="${KB_HEALTH_DRAIN_TIMEOUT:-600}"

BOT_TOKEN=""
CHAT_ID=""
# shellcheck source=/dev/null
[ -r "$ENV_FILE" ] && . "$ENV_FILE"

ts() { date '+%Y-%m-%d %H:%M:%S'; }
log() { printf '%s [kb-health] %s\n' "$(ts)" "$*"; }

notify() {
    local text="$1"
    if [ -z "$BOT_TOKEN" ] || [ -z "$CHAT_ID" ]; then
        log "UPOZORENJE: Telegram nije konfigurisan ($ENV_FILE), alarm nije poslat"
        return 1
    fi
    "$CURL" -sS -m 20 -o /dev/null \
        --data-urlencode "chat_id=${CHAT_ID}" \
        --data-urlencode "text=${text}" \
        "https://api.telegram.org/bot${BOT_TOKEN}/sendMessage"
}

failures=""
if [ -n "${KB_HEALTH_PY:-}" ]; then
    log "zastareli KB_HEALTH_PY je odbijen"
    failures+="[config] KB_HEALTH_PY je uklonjen; postavi odvojeno KB_HEALTH_PY_EMBED i KB_HEALTH_PY_SEARCH"$'\n'
fi

# Proveri pune dependency root-ove pre poziva koji ih ucitavaju lenjo. Posle
# migracije svaka grupa ide kroz svoj venv; do tada oba podrazumevano koriste
# isti interpreter pod istim turok nalogom kao produkcioni servisi.
if import_health=$("$PY_EMBED" -c 'import chromadb, fastembed' 2>&1); then
    log "embed Python importi OK"
else
    log "embed Python importi PALI"
    failures+="[embed-python] import: ${import_health:0:600}"$'\n'
fi

if import_health=$("$PY_SEARCH" -c \
    'import fastapi, httpx, pydantic, sentence_transformers, uvicorn, yaml' 2>&1); then
    log "search Python importi OK"
else
    log "search Python importi PALI"
    failures+="[search-python] import: ${import_health:0:600}"$'\n'
fi

for corpus in "${CORPORA[@]}"; do
    # 1) isprazni pending — pokriva i unose zaostale dok je watcher bio ugasen
    drain=$("$TIMEOUT" "$DRAIN_TIMEOUT" "$PY_EMBED" "$COMPILE" --corpus "$corpus" 2>&1)
    drain_rc=$?
    if [ "$drain_rc" -ne 0 ]; then
        log "compile PAO za $corpus (rc=$drain_rc)"
        failures+="[$corpus] compile (rc=$drain_rc): ${drain:0:400}"$'\n'
        continue
    fi

    # 2) proveri invarijante SQLite <-> Chroma.
    # exit 3 je ugovor iz compile.py: mutirajuci proces je drzao lock ceo budzet
    # (HEALTH_LOCK_TIMEOUT), pa je stanje nepoznato a ne nezdravo — preskace se
    # bez alarma. Razlikuje se ISKLJUCIVO po exit kodu, nikad grep-om po tekstu.
    health=$("$PY_EMBED" "$COMPILE" --corpus "$corpus" --health 2>&1)
    health_rc=$?
    if [ "$health_rc" -eq 0 ]; then
        log "$corpus OK"
    elif [ "$health_rc" -eq 3 ]; then
        log "$corpus health: busy (mutacija u toku) — preskoceno, bez alarma"
    else
        log "$corpus NEZDRAV (rc=$health_rc)"
        failures+="[$corpus] health (rc=$health_rc): ${health:0:600}"$'\n'
    fi
done

# 3) korpusi mogu biti zdravi dok API radi bez obaveznog rerankera. Taj kvar
# menja semantiku pretrage, pa ga tretiramo kao pad servisa, ne kao degradaciju.
if search_health=$("$CURL" -fsS -m 20 "$SEARCH_HEALTH_URL" 2>&1); then
    rerank_model=$(printf '%s' "$search_health" | "$PY_SEARCH" -c \
        'import json,sys; value=json.load(sys.stdin).get("rerank_model"); print(value if isinstance(value, str) else "")' \
        2>/dev/null) || rerank_model=""
    if [ -n "$rerank_model" ]; then
        log "reranker OK ($rerank_model)"
    else
        log "reranker NIJE UČITAN"
        failures+="[search-api] /health nema učitan rerank_model"$'\n'
    fi
else
    log "search API health PAO"
    failures+="[search-api] /health: ${search_health:0:400}"$'\n'
fi

if [ -n "$failures" ]; then
    notify "KB health FAIL na Nexusu — $(ts)"$'\n\n'"$failures"
    printf '%s' "$failures" >&2
    exit 1
fi

log "oba Python okruženja, oba corpusa i reranker zdravi"
exit 0
