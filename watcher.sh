#!/bin/bash
# kb-watcher: inotify na /opt/kb/raw/, auto-pokreće compile.py
# Systemd: /etc/systemd/system/kb-watcher.service

RAWDIR="/opt/kb/raw"
COMPILE="/usr/bin/python3 /opt/kb/compile.py"
LOCK="/tmp/kb-watcher.lock"
LAST_FILE="/tmp/kb-watcher-last"
MIN_INTERVAL=5  # sekundi između kompajliranja

inotifywait -m -r -e close_write -e moved_to --format "%w%f" "$RAWDIR" 2>/dev/null |
while read -r filepath; do
    # Samo .md fajlovi
    [[ "$filepath" != *.md ]] && continue

    now=$(date +%s)
    last=$(cat "$LAST_FILE" 2>/dev/null || echo 0)
    elapsed=$(( now - last ))

    if (( elapsed < MIN_INTERVAL )); then
        sleep $(( MIN_INTERVAL - elapsed ))
    fi

    # Lock da ne bi dva compile-a išla paralelno
    (
        flock -n 200 || exit 0
        date +%s > "$LAST_FILE"
        $COMPILE
    ) 200>"$LOCK"
done
