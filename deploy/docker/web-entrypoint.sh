#!/bin/sh
set -eu

runtime_config=/tmp/runtime-config.js
api_base_url=${API_BASE_URL:-}

fail() {
    printf 'firebreak-web: %s\n' "$1" >&2
    exit 64
}

[ -n "$api_base_url" ] || fail 'API_BASE_URL is required (expected /api or an absolute http(s) URL)'

case "$api_base_url" in
    /*|http://*|https://*) ;;
    *) fail 'API_BASE_URL must start with /, http://, or https://' ;;
esac

printf '%s\n' "$api_base_url" | grep -Eq '^(/|https?://)[A-Za-z0-9._~:/@!$&()*+,;=%-]+$' || \
    fail 'API_BASE_URL contains unsupported characters'

while [ "$api_base_url" != "/" ] && [ "${api_base_url%/}" != "$api_base_url" ]; do
    api_base_url=${api_base_url%/}
done

umask 077
printf '%s\n' \
    "window.__FIREBREAK_CONFIG__ = Object.freeze({ apiBaseUrl: \"$api_base_url\" });" \
    >"$runtime_config"

case "${1:-}" in
    '') exec nginx -g 'daemon off;' ;;
    --check) exec nginx -t ;;
    *) fail "unsupported argument: $1" ;;
esac
