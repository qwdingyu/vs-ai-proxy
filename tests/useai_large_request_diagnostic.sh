#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

CONFIG_SOURCE="${CONFIG_SOURCE:-$HOME/.config/vs-ai-proxy/config.json}"
INJECT_MODEL_BINDING="${INJECT_MODEL_BINDING:-1}"
PROXY_PORT="${PROXY_PORT:-12346}"
MODEL="${MODEL:-deepseek-v4-flash}"
PROVIDER_ID="${PROVIDER_ID:-useai}"
DISPLAY_PROVIDER="${DISPLAY_PROVIDER:-UseAI}"
TARGET_BYTES="${TARGET_BYTES:-1060000}"
PAYLOAD_SHAPE="${PAYLOAD_SHAPE:-message}"
DIRECT_TIMEOUT_SECONDS="${DIRECT_TIMEOUT_SECONDS:-90}"
PROXY_TIMEOUT_SECONDS="${PROXY_TIMEOUT_SECONDS:-120}"
MODE="${MODE:-direct-proxy}"
COOLDOWN_SECONDS="${COOLDOWN_SECONDS:-0}"

RESULT_DIR="$ROOT_DIR/.bin/useai-large-diagnostic"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/vs-ai-proxy-diagnostic.XXXXXX")"
PROXY_BIN="$WORK_DIR/vs-ai-proxy"
CONFIG_PATH="$WORK_DIR/config.json"
AUTH_HEADER_PATH="$WORK_DIR/auth-header.txt"
REQUEST_PATH="$WORK_DIR/request.json"
PROXY_REQUEST_PATH="$WORK_DIR/proxy-request.json"
DIRECT_OUTPUT="$WORK_DIR/direct.out"
DIRECT_ERROR="$WORK_DIR/direct.err"
DIRECT_META="$WORK_DIR/direct.meta"
PROXY_OUTPUT="$WORK_DIR/proxy.out"
PROXY_ERROR="$WORK_DIR/proxy.err"
PROXY_META="$WORK_DIR/proxy.meta"
PROXY_LOG="$WORK_DIR/proxy.log"
PID_FILE="$WORK_DIR/proxy.pid"
STORE_PATH="$WORK_DIR/logs.json"
REQUEST_STATS_PATH="$WORK_DIR/request-stats.json"
RESULT_PATH="$RESULT_DIR/result.json"

umask 077
mkdir -p "$RESULT_DIR"
chmod 700 "$WORK_DIR" "$RESULT_DIR"

stop_proxy() {
  if [[ -f "$PID_FILE" ]]; then
    local pid
    pid="$(<"$PID_FILE")"
    kill "$pid" >/dev/null 2>&1 || true
    for _ in {1..40}; do
      if ! kill -0 "$pid" >/dev/null 2>&1; then
        break
      fi
      sleep 0.25
    done
  fi
}

cleanup() {
  stop_proxy
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

if [[ ! -f "$CONFIG_SOURCE" ]]; then
  echo "配置文件不存在: $CONFIG_SOURCE" >&2
  exit 1
fi
if [[ "$PAYLOAD_SHAPE" != "message" && "$PAYLOAD_SHAPE" != "tools" ]]; then
  echo "PAYLOAD_SHAPE 仅支持 message 或 tools" >&2
  exit 1
fi
if [[ "$MODE" != "direct-proxy" && "$MODE" != "proxy-only" && "$MODE" != "direct-only" ]]; then
  echo "MODE 仅支持 direct-proxy、proxy-only 或 direct-only" >&2
  exit 1
fi

python3 - "$CONFIG_SOURCE" "$CONFIG_PATH" "$PROXY_PORT" "$PROVIDER_ID" "$MODEL" \
  "$REQUEST_PATH" "$TARGET_BYTES" "$INJECT_MODEL_BINDING" "$PAYLOAD_SHAPE" "$REQUEST_STATS_PATH" <<'PY'
import json
import os
import sys

(
    config_source,
    config_path,
    port,
    provider_id,
    model,
    request_path,
    target_bytes,
    inject_model_binding,
    payload_shape,
    request_stats_path,
) = sys.argv[1:]
target_bytes = int(target_bytes)
inject_model_binding = inject_model_binding.strip().lower() not in ('0', 'false', 'no', 'off')

with open(config_source, encoding='utf-8') as f:
    cfg = json.load(f)
cfg['port'] = int(port)
if inject_model_binding:
    models = cfg.setdefault('models', [])
    for item in models:
        same_model = str(item.get('name', '')).lower() == model.lower()
        item_provider = str(item.get('provider_id') or item.get('provider') or '').lower()
        if same_model and item_provider == provider_id.lower():
            item['enabled'] = True
            break
    else:
        models.append({
            'name': model,
            'provider_id': provider_id,
            'provider': provider_id,
            'supports_tools': True,
            'enabled': True,
        })
with open(config_path, 'w', encoding='utf-8') as f:
    json.dump(cfg, f, ensure_ascii=False, indent=2)
os.chmod(config_path, 0o600)

tool_names = [
    'adapt_plan', 'ask_question', 'clarify_requirements', 'code_search', 'create_file',
    'detect_memories', 'file_search', 'find_symbol', 'get_file', 'edit_file',
    'apply_patch', 'powershell', 'git', 'list_files', 'read_file', 'replace_in_file',
    'run_command', 'search_symbols', 'update_plan', 'write_file',
]
tools = []
for name in tool_names:
    tools.append({
        'type': 'function',
        'function': {
            'name': name,
            'description': ('VS/Copilot diagnostic tool ' + name + ' ') * 16,
            'parameters': {
                'type': 'object',
                'properties': {
                    'path': {'type': 'string', 'description': 'Repository-relative file path.'},
                    'content': {'type': 'string', 'description': 'Complete file content.'},
                    'command': {'type': 'string', 'description': 'Command to execute.'},
                    'query': {'type': 'string', 'description': 'Search query.'},
                },
                'additionalProperties': True,
            },
        },
    })

body = {
    'model': model,
    'messages': [
        {'role': 'system', 'content': 'You are Visual Studio Copilot coding assistant.'},
        {'role': 'user', 'content': 'Reply with one short sentence. Do not call tools.'},
    ],
    'tools': tools,
    'tool_choice': 'auto',
    'stream': True,
    'max_tokens': 64,
    'temperature': 1.0,
}

def encoded(value):
    return json.dumps(value, ensure_ascii=False, separators=(',', ':')).encode('utf-8')

current = len(encoded(body))
if current > target_bytes:
    raise SystemExit(f'TARGET_BYTES={target_bytes} is smaller than base request={current}')
filler_bytes = target_bytes - current
if payload_shape == 'message':
    body['messages'][1]['content'] += 'x' * filler_bytes
else:
    share, remainder = divmod(filler_bytes, len(body['tools']))
    for index, tool in enumerate(body['tools']):
        tool['function']['description'] += 'x' * (share + (1 if index < remainder else 0))

raw = encoded(body)
with open(request_path, 'wb') as f:
    f.write(raw)
os.chmod(request_path, 0o600)

stats = {
    'payload_shape': payload_shape,
    'request_bytes': len(raw),
    'messages_bytes': len(encoded(body['messages'])),
    'tools_bytes': len(encoded(body['tools'])),
    'tool_count': len(body['tools']),
    'model': model,
    'provider_id': provider_id,
}
with open(request_stats_path, 'w', encoding='utf-8') as f:
    json.dump(stats, f, ensure_ascii=False)
print(json.dumps(stats, ensure_ascii=False))
PY

BASE_URL="$(python3 - "$CONFIG_PATH" "$PROVIDER_ID" "$AUTH_HEADER_PATH" <<'PY'
import json
import os
import sys

config_path, provider_id, auth_header_path = sys.argv[1:]
with open(config_path, encoding='utf-8') as f:
    cfg = json.load(f)
provider_id = provider_id.lower()
for provider in cfg.get('providers', []):
    key = (provider.get('id') or provider.get('name') or '').lower()
    if key == provider_id or (provider.get('name') or '').lower() == provider_id:
        api_key = provider.get('api_key', '')
        if not api_key:
            raise SystemExit('provider API key is empty: ' + provider_id)
        with open(auth_header_path, 'w', encoding='utf-8') as f:
            f.write('Authorization: Bearer ' + api_key + '\n')
        os.chmod(auth_header_path, 0o600)
        print(provider.get('base_url', '').rstrip('/'))
        break
else:
    raise SystemExit('provider not found: ' + provider_id)
PY
)"

printf '\n== 1. direct upstream: %s/chat/completions ==\n' "$BASE_URL"
DIRECT_RC=0
if [[ "$MODE" == "proxy-only" ]]; then
  printf 'skip direct upstream because MODE=proxy-only\n'
else
set +e
curl -sS -N --max-time "$DIRECT_TIMEOUT_SECONDS" \
  -o "$DIRECT_OUTPUT" \
  -w '%{http_code}\n%{time_starttransfer}\n%{time_total}\n%{size_upload}\n%{size_download}\n' \
  -H 'Content-Type: application/json' \
  -H 'User-Agent: VS-AI-Proxy-Diagnostic/2.0' \
  -H "@$AUTH_HEADER_PATH" \
  --data-binary "@$REQUEST_PATH" \
  "$BASE_URL/chat/completions" >"$DIRECT_META" 2>"$DIRECT_ERROR"
DIRECT_RC=$?
set -e
fi

if [[ "$MODE" == "direct-only" ]]; then
  PROXY_RC=0
  python3 - "$REQUEST_STATS_PATH" "$STORE_PATH" "$RESULT_PATH" \
    "$DIRECT_META" "$DIRECT_OUTPUT" "$DIRECT_ERROR" "$DIRECT_RC" \
    "$PROXY_META" "$PROXY_OUTPUT" "$PROXY_ERROR" "$PROXY_RC" <<'PY'
import json
import os
import sys

(
    request_stats_path,
    store_path,
    result_path,
    direct_meta,
    direct_output,
    direct_error,
    direct_rc,
    proxy_meta,
    proxy_output,
    proxy_error,
    proxy_rc,
) = sys.argv[1:]

def milliseconds(value):
    try:
        return round(float(value) * 1000, 3)
    except (TypeError, ValueError):
        return None

def curl_result(meta_path, output_path, error_path, return_code):
    lines = []
    if os.path.exists(meta_path):
        with open(meta_path, encoding='utf-8') as f:
            lines = [line.strip() for line in f]
    lines += [''] * (5 - len(lines))
    raw = b''
    if os.path.exists(output_path):
        with open(output_path, 'rb') as f:
            raw = f.read()
    stderr_kind = None
    if os.path.exists(error_path) and os.path.getsize(error_path):
        stderr_kind = 'curl_error'
    return {
        'curl_rc': int(return_code),
        'status': lines[0] or '000',
        'ttfb_ms': milliseconds(lines[1]),
        'total_ms': milliseconds(lines[2]),
        'upload_bytes': int(float(lines[3])) if lines[3] else None,
        'download_bytes': int(float(lines[4])) if lines[4] else None,
        'sse_events': sum(1 for line in raw.splitlines() if line.startswith(b'data:')),
        'sse_done': any(line.strip() == b'data: [DONE]' for line in raw.splitlines()),
        'error_kind': stderr_kind,
    }

with open(request_stats_path, encoding='utf-8') as f:
    result = json.load(f)
result['mode'] = 'direct-only'
result['direct'] = curl_result(direct_meta, direct_output, direct_error, direct_rc)
result['proxy'] = {'skipped': True}
result['proxy_diagnostic'] = {}
with open(result_path, 'w', encoding='utf-8') as f:
    json.dump(result, f, ensure_ascii=False, indent=2)
os.chmod(result_path, 0o600)
print(json.dumps(result, ensure_ascii=False, indent=2))
PY
  printf '\n脱敏诊断结果已保存: %s\n' "$RESULT_PATH"
  exit 0
fi

printf '\n== 2. local proxy: build and start ==\n'
go build -o "$PROXY_BIN" ./cmd/server
VS_AI_PROXY_AUTO_UPDATE=0 CONFIG_PATH="$CONFIG_PATH" STORE_PATH="$STORE_PATH" "$PROXY_BIN" >"$PROXY_LOG" 2>&1 &
echo $! > "$PID_FILE"
for _ in {1..80}; do
  if curl -sf "http://127.0.0.1:$PROXY_PORT/health" >/dev/null; then
    break
  fi
  sleep 0.25
done
curl -sf "http://127.0.0.1:$PROXY_PORT/health" >/dev/null

python3 - "$REQUEST_PATH" "$PROXY_REQUEST_PATH" "$DISPLAY_PROVIDER" <<'PY'
import json
import os
import sys

src, dst, display_provider = sys.argv[1:]
with open(src, encoding='utf-8') as f:
    body = json.load(f)
body['model'] = f'{display_provider} - {body["model"]}'
# Kimi only accepts 1. The proxy must govern this client value while other providers preserve it.
body['temperature'] = 0.2
with open(dst, 'wb') as f:
    f.write(json.dumps(body, ensure_ascii=False, separators=(',', ':')).encode('utf-8'))
os.chmod(dst, 0o600)
PY

if [[ "$COOLDOWN_SECONDS" =~ ^[0-9]+$ && "$COOLDOWN_SECONDS" -gt 0 ]]; then
  printf '\n== cooldown: sleep %ss before proxy request ==\n' "$COOLDOWN_SECONDS"
  sleep "$COOLDOWN_SECONDS"
fi

printf '\n== 3. proxy request: /v1/chat/completions ==\n'
set +e
curl -sS -N --max-time "$PROXY_TIMEOUT_SECONDS" \
  -o "$PROXY_OUTPUT" \
  -w '%{http_code}\n%{time_starttransfer}\n%{time_total}\n%{size_upload}\n%{size_download}\n' \
  -H 'Content-Type: application/json' \
  --data-binary "@$PROXY_REQUEST_PATH" \
  "http://127.0.0.1:$PROXY_PORT/v1/chat/completions" >"$PROXY_META" 2>"$PROXY_ERROR"
PROXY_RC=$?
set -e

stop_proxy

python3 - "$REQUEST_STATS_PATH" "$STORE_PATH" "$RESULT_PATH" \
  "$DIRECT_META" "$DIRECT_OUTPUT" "$DIRECT_ERROR" "$DIRECT_RC" \
  "$PROXY_META" "$PROXY_OUTPUT" "$PROXY_ERROR" "$PROXY_RC" <<'PY'
import json
import os
import sys

(
    request_stats_path,
    store_path,
    result_path,
    direct_meta,
    direct_output,
    direct_error,
    direct_rc,
    proxy_meta,
    proxy_output,
    proxy_error,
    proxy_rc,
) = sys.argv[1:]

def milliseconds(value):
    try:
        return round(float(value) * 1000, 3)
    except (TypeError, ValueError):
        return None

def curl_result(meta_path, output_path, error_path, return_code):
    lines = []
    if os.path.exists(meta_path):
        with open(meta_path, encoding='utf-8') as f:
            lines = [line.strip() for line in f]
    lines += [''] * (5 - len(lines))
    raw = b''
    if os.path.exists(output_path):
        with open(output_path, 'rb') as f:
            raw = f.read()
    stderr_kind = None
    if os.path.exists(error_path) and os.path.getsize(error_path):
        stderr_kind = 'curl_error'
    return {
        'curl_rc': int(return_code),
        'status': lines[0] or '000',
        'ttfb_ms': milliseconds(lines[1]),
        'total_ms': milliseconds(lines[2]),
        'upload_bytes': int(float(lines[3])) if lines[3] else None,
        'download_bytes': int(float(lines[4])) if lines[4] else None,
        'sse_events': sum(1 for line in raw.splitlines() if line.startswith(b'data:')),
        'sse_done': any(line.strip() == b'data: [DONE]' for line in raw.splitlines()),
        'error_kind': stderr_kind,
    }

with open(request_stats_path, encoding='utf-8') as f:
    result = json.load(f)
mode = os.environ.get('MODE', 'direct-proxy')
result['mode'] = mode
try:
    result['cooldown_seconds'] = int(os.environ.get('COOLDOWN_SECONDS', '0'))
except ValueError:
    result['cooldown_seconds'] = 0
result['direct'] = curl_result(direct_meta, direct_output, direct_error, direct_rc)
if mode == 'proxy-only':
    result['direct']['skipped'] = True
result['proxy'] = curl_result(proxy_meta, proxy_output, proxy_error, proxy_rc)

logs = []
if os.path.exists(store_path):
    with open(store_path, encoding='utf-8') as f:
        logs = json.load(f)
chat_logs = [
    item for item in logs
    if item.get('method') == 'POST' and item.get('path') == '/v1/chat/completions'
]
last = chat_logs[-1] if chat_logs else {}
result['proxy_diagnostic'] = {
    key: last.get(key)
    for key in (
        'status_code', 'elapsed_ms', 'request_bytes', 'upstream_bytes', 'stream_state',
        'error_code', 'attempts_summary', 'request_tools',
    )
    if last.get(key) is not None
}

with open(result_path, 'w', encoding='utf-8') as f:
    json.dump(result, f, ensure_ascii=False, indent=2)
os.chmod(result_path, 0o600)
print(json.dumps(result, ensure_ascii=False, indent=2))
PY

printf '\n脱敏诊断结果已保存: %s\n' "$RESULT_PATH"
