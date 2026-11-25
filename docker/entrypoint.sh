#!/bin/sh
# The proxy writes its query log to stdout and its service (error) log to
# stderr. Here both streams are redirected to files in DBPROXY_LOG_DIR, which
# is expected to be a directory mounted from the host.
# Set DBPROXY_LOG_DIR to an empty value to keep the logs in `docker logs`.
set -eu

if [ -n "${DBPROXY_LOG_DIR:-}" ]; then
    exec /usr/local/bin/db-proxy-ro \
        >>"${DBPROXY_LOG_DIR}/query.log" \
        2>>"${DBPROXY_LOG_DIR}/error.log"
fi
exec /usr/local/bin/db-proxy-ro
