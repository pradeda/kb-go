#!/bin/bash
# kb-watcher: inotify na /opt/kb/raw/, auto-pokreće compile.py
# Systemd: /etc/systemd/system/kb-watcher.service

RAWDIR="/opt/kb/raw"
COMPILE="/usr/bin/python3 /opt/kb/compile.py"
LOCK="/tmp/kb-watcher.lock"
MIN_INTERVAL=5  # sekundi između kompajliranja

LAST_COMPILE=0

inotifywait -m -r -e close_write -e moved_to --format "%w%f" "$RAWDIR" 2>/dev/null |
while read -r filepath; do
    # Samo .md fajlovi
    [[ "$filepath" != *.md ]] && continue

    now=$(date +%s)
    elapsed=$(( now - LAST_COMPILE ))

    if (( elapsed < MIN_INTERVAL )); then
        # Sačekaj da protekne interval od poslednjeg kompajliranja
        wait=$(( MIN_INTERVAL - elapsed ))
        sleep "$wait"
    fi

    # Lock da ne bi dva compile-a išla paralelno
    (
        flock -n 200 || exit 0
        LAST_COMPILE=$(date +%s)
        $COMPILE
    ) 200>"$LOCK"
done
