#!/bin/bash
# kb-watcher: watches /opt/kb/raw/ via inotify, triggers compile.py automatically
# Systemd: /etc/systemd/system/kb-watcher.service

RAWDIR="/opt/kb/raw"
COMPILE="/usr/bin/python3 /opt/kb/compile.py"
LOCK="/tmp/kb-watcher.lock"
LAST_FILE="/tmp/kb-watcher-last"
MIN_INTERVAL=5  # seconds between compilations

inotifywait -m -r -e close_write -e moved_to --format "%w%f" "$RAWDIR" 2>/dev/null |
while read -r filepath; do
    # Only .md files
    [[ "$filepath" != *.md ]] && continue

    now=$(date +%s)
    last=$(cat "$LAST_FILE" 2>/dev/null || echo 0)
    elapsed=$(( now - last ))

    if (( elapsed < MIN_INTERVAL )); then
        sleep $(( MIN_INTERVAL - elapsed ))
    fi

    # Blocking lock: event that arrives mid-compile waits for its own run
    # instead of being dropped (entry would stay unembedded until a future event)
    (
        flock 200
        date +%s > "$LAST_FILE"
        $COMPILE
    ) 200>"$LOCK"
done
