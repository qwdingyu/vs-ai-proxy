#!/usr/bin/env bash
# Local build/start/smoke-test helper for VS AI Proxy.
#
# Usage:
#   ./build-test.sh         build + restart + smoke test
#   ./build-test.sh build   build only
#   ./build-test.sh start   restart only
#   ./build-test.sh test    smoke test current process
#   ./build-test.sh stop    stop process started by this script
#
# Environment:
#   PORT=12345              single HTTP port
#   HOST=127.0.0.1          local bind address; use 0.0.0.0 on cloud hosts
#   CONFIG_PATH=...         optional config.json path; defaults to app user config
#   ADMIN_API_KEY=...       optional /admin/api bearer token
#   PROXY_API_KEY=...       optional proxy token; also protects /admin/api if ADMIN_API_KEY is empty

set -euo pipefail

APP_NAME="${APP_NAME:-server}"
PORT="${PORT:-12345}"
HOST="${HOST:-127.0.0.1}"
BASE_URL="${BASE_URL:-http://127.0.0.1:${PORT}}"
PID_FILE="${PID_FILE:-/tmp/vs-ai-proxy-test-${PORT}.pid}"
LOG_FILE="${LOG_FILE:-/tmp/vs-ai-proxy-test-${PORT}.log}"
START_TIMEOUT_SECONDS="${START_TIMEOUT_SECONDS:-15}"

admin_token() {
    if [ -n "${ADMIN_API_KEY:-}" ]; then
        printf '%s' "${ADMIN_API_KEY}"
        return
    fi
    if [ -n "${PROXY_API_KEY:-}" ]; then
        printf '%s' "${PROXY_API_KEY}"
    fi
}

curl_status() {
    curl -sS -o /dev/null -w '%{http_code}' "$@"
}

build() {
    echo "Building ${APP_NAME}..."
    go build -o "${APP_NAME}" ./cmd/server
    echo "Build complete: ./${APP_NAME}"
}

stop() {
    if [ -f "${PID_FILE}" ]; then
        old_pid="$(cat "${PID_FILE}")"
        if kill -0 "${old_pid}" 2>/dev/null; then
            echo "Stopping old process (PID ${old_pid})..."
            kill "${old_pid}" 2>/dev/null || true
            for _ in $(seq 1 20); do
                if ! kill -0 "${old_pid}" 2>/dev/null; then
                    break
                fi
                sleep 0.1
            done
        fi
        rm -f "${PID_FILE}"
    fi

    # Fallback cleanup for the selected single port only.
    if command -v lsof >/dev/null 2>&1; then
        pids="$(lsof -tiTCP:"${PORT}" -sTCP:LISTEN 2>/dev/null || true)"
        if [ -n "${pids}" ]; then
            echo "Killing process(es) still listening on port ${PORT}: ${pids}"
            # shellcheck disable=SC2086
            kill ${pids} 2>/dev/null || true
            sleep 0.5
        fi
    fi
}

wait_until_ready() {
    deadline=$((SECONDS + START_TIMEOUT_SECONDS))
    while [ "${SECONDS}" -lt "${deadline}" ]; do
        if [ "$(curl_status "${BASE_URL}/health" 2>/dev/null || true)" = "200" ]; then
            return 0
        fi
        sleep 0.2
    done
    echo "Timed out waiting for ${BASE_URL}/health"
    return 1
}

start() {
    stop
    echo "Starting ${APP_NAME} on ${HOST}:${PORT}..."
    echo "Log file: ${LOG_FILE}"
    : > "${LOG_FILE}"
    HOST="${HOST}" PORT="${PORT}" nohup ./"${APP_NAME}" >"${LOG_FILE}" 2>&1 &
    new_pid=$!
    disown "${new_pid}" 2>/dev/null || true
    echo "${new_pid}" > "${PID_FILE}"

    if ! wait_until_ready; then
        echo "Startup failed. Last server log lines:"
        tail -n 80 "${LOG_FILE}" || true
        exit 1
    fi

    echo "Started successfully (PID ${new_pid})"
    echo "Proxy base: ${BASE_URL}"
    echo "Admin UI:   ${BASE_URL}/admin"
    rg -n "运行配置文件|请求日志文件|已加载环境配置文件|监听地址" "${LOG_FILE}" || true
}

assert_status() {
    expected="$1"
    label="$2"
    shift 2

    actual="$(curl_status "$@" 2>/dev/null || true)"
    if [ "${actual}" != "${expected}" ]; then
        echo "Smoke test failed: ${label}: got HTTP ${actual}, want ${expected}"
        if [ -f "${LOG_FILE}" ]; then
            echo "Last server log lines:"
            tail -n 40 "${LOG_FILE}" || true
        fi
        exit 1
    fi
    echo "OK ${label}: HTTP ${actual}"
}

smoke_test() {
    echo "Running smoke tests against ${BASE_URL}..."
    assert_status "200" "health" "${BASE_URL}/health"
    assert_status "200" "admin html" "${BASE_URL}/admin"
    assert_status "200" "openai models" "${BASE_URL}/v1/models"
    assert_status "200" "ollama tags" "${BASE_URL}/api/tags"
    assert_status "404" "root /api/config must not be management API" "${BASE_URL}/api/config"

    token="$(admin_token)"
    if [ -n "${token}" ]; then
        assert_status "401" "admin api without token" "${BASE_URL}/admin/api/config"
        assert_status "200" "admin api with token" -H "Authorization: Bearer ${token}" "${BASE_URL}/admin/api/config"
    else
        assert_status "200" "admin api without token" "${BASE_URL}/admin/api/config"
    fi

    config_json="$(mktemp)"
    if [ -n "${token}" ]; then
        curl -fsS -H "Authorization: Bearer ${token}" "${BASE_URL}/admin/api/config" > "${config_json}"
    else
        curl -fsS "${BASE_URL}/admin/api/config" > "${config_json}"
    fi
    python3 - <<'PY' "${config_json}"
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    cfg = json.load(f)
providers = cfg.get("providers") or []
models = cfg.get("models") or []
print(f"Config summary: port={cfg.get('port')} providers={len(providers)} models={len(models)}")
PY
    rm -f "${config_json}"

    echo "Smoke tests passed."
}

case "${1:-}" in
    build)
        build
        ;;
    start)
        start
        ;;
    test)
        smoke_test
        ;;
    stop)
        stop
        ;;
    "")
        build
        start
        smoke_test
        ;;
    *)
        echo "Usage: $0 [build|start|test|stop]"
        exit 1
        ;;
esac
