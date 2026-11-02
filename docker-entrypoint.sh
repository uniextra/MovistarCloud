#!/bin/sh
set -e

TOKENS_DIR="${TOKENS_DIR:-/app/tokens}"
export TOKENS_DIR

can_access_as_app() {
    su-exec app:app sh -c '
        test -r "$1" && test -w "$1" || exit 1
        for f in "$1"/.env "$1"/*.env "$1"/*.sqlite*; do
            if [ -e "$f" ]; then
                test -r "$f" && test -w "$f" || exit 1
            fi
        done
    ' -- "$TOKENS_DIR"
}

# If running as root (e.g. host volume mounted from Synology DSM / Unraid / Docker),
# attempt to grant ownership of $TOKENS_DIR to user "app" (UID 10001).
# If the volume permissions or mount options prevent access as "app",
# fall back to running as root to preserve user data and operational stability,
# strictly adhering to the Docker non-root security guidelines.
if [ "$(id -u)" = "0" ]; then
    mkdir -p "$TOKENS_DIR"
    chown -R app:app "$TOKENS_DIR" 2>/dev/null || true
    chmod 700 "$TOKENS_DIR" 2>/dev/null || true
    chmod 600 "$TOKENS_DIR"/.env "$TOKENS_DIR"/*.env 2>/dev/null || true

    if can_access_as_app; then
        exec su-exec app:app /app/movistar-uploader "$@"
    else
        exec /app/movistar-uploader "$@"
    fi
fi

exec /app/movistar-uploader "$@"
