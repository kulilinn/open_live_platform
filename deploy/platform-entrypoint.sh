#!/bin/sh
set -eu

: "${DATABASE_URL:?Set DATABASE_URL for the PostgreSQL deployment}"
: "${JWT_SECRET:?Set JWT_SECRET before starting the service}"

if [ "${1-}" != create_pass ]; then
    : "${RELAY_SECRET:?Set RELAY_SECRET before starting the service}"
    if [ "$RELAY_SECRET" = dev-secret ]; then
        printf '%s\n' 'Set RELAY_SECRET to a deployment secret.' >&2
        exit 1
    fi
fi

exec /usr/local/bin/platform "$@"
