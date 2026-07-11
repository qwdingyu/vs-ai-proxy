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
DIRECT_TIMEOUT_SECONDS="${DIRECT_TIMEOUT_SECONDS:-90}"
PROXY_TIMEOUT_SECONDS="${PROXY_TIMEOUT_SECONDS:-120}"

WORK_DIR="$ROOT_DIR/.bin/useai-large-diagnostic"
PROXY_BIN="$WORK_DIR/vs-ai-proxy"
CONFIG_PATH="$WORK_DIR/config.json"
REQUEST_PATH="$WORK_DIR/request.json"
DIRECT_OUTPUT="$WORK_DIR/direct.out"
PROXY_OUTPUT="$WORK_DIR/proxy.out"
PROXY_LOG="$WORK_DIR/proxy.log"
PID_FILE="$WORK_DIR/proxy.pid"
RESULT_PATH="$WORK_DIR/result.json"
STORE_PATH="$WORK_DIR/logs.json"

mkdir -p "$WORK_DIR"
rm -f "$DIRECT_OUTPUT" "$PROXY_OUTPUT" "$PROXY_LOG" "$PID_FILE" "$RESULT_PATH" "$STORE_PATH"

stop_proxy() {
  if [[ -f "$PID_FILE" ]]; then
    local pid
    pid="$(cat "$PID_FILE")"
    kill "$pid" >/dev/null 2>&1 || true
    for _ in {1..40}; do
      if ! kill -0 "$pid" >/dev/null 2>&1; then
        break
      fi
      sleep 0.25
    done
    rm -f "$PID_FILE"
  fi
}
cleanup() {
  stop_proxy
}
trap cleanup EXIT

if [[ ! -f "$CONFIG_SOURCE" ]]; then
  echo "配置文件不存在: $CONFIG_SOURCE" >&2
  exit 1
fi

python3 - "$CONFIG_SOURCE" "$CONFIG_PATH" "$PROXY_PORT" "$PROVIDER_ID" "$MODEL" "$REQUEST_PATH" "$TARGET_BYTES" "$INJECT_MODEL_BINDING" <<'PY'
import json
import sys

config_source, config_path, port, provider_id, model, request_path, target_bytes, inject_model_binding = sys.argv[1:]
target_bytes = int(target_bytes)
inject_model_binding = inject_model_binding.strip().lower() not in ('0', 'false', 'no', 'off')
with open(config_source, encoding='utf-8') as f:
    cfg = json.load(f)
cfg['port'] = int(port)
# 诊断脚本默认在临时配置中把目标模型绑定到目标 provider，
# 用于测试 vs-ai-proxy 的真实出站 JSON；不会修改用户原始 config.json。
if inject_model_binding:
    models = cfg.setdefault('models', [])
    exists = False
    for item in models:
        if str(item.get('name','')).lower() == model.lower() and str(item.get('provider_id') or item.get('provider') or '').lower() == provider_id.lower():
            exists = True
            item['enabled'] = True
            break
    if not exists:
        models.append({'name': model, 'provider_id': provider_id, 'provider': provider_id, 'supports_tools': True, 'enabled': True})
with open(config_path, 'w', encoding='utf-8') as f:
    json.dump(cfg, f, ensure_ascii=False, indent=2)

tool_names = [
    'adapt_plan','ask_question','clarify_requirements','code_search','create_file','detect_memories',
    'file_search','find_symbol','get_file','edit_file','apply_patch','powershell','git','list_files',
    'read_file','replace_in_file','run_command','search_symbols','update_plan','write_file'
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
                    'path': {'type': 'string'},
                    'content': {'type': 'string'},
                    'command': {'type': 'string'},
                    'query': {'type': 'string'},
                },
                'additionalProperties': True,
            },
        },
    })

base_body = {
    'model': model,
    'messages': [
        {'role': 'system', 'content': 'You are Visual Studio Copilot coding assistant.'},
        {'role': 'user', 'content': '请根据当前代码创建/修改文件。\n'},
    ],
    'tools': tools,
    'tool_choice': 'auto',
    'stream': True,
    'max_tokens': 4096,
}

def encoded_len(body):
    return len(json.dumps(body, ensure_ascii=False, separators=(',', ':')).encode('utf-8'))

line = '// large context line: abcdefghijklmnopqrstuvwxyz 0123456789\n'
content = base_body['messages'][1]['content']
while True:
    base_body['messages'][1]['content'] = content
    current = encoded_len(base_body)
    if current >= target_bytes:
        break
    missing = target_bytes - current
    content += line * max(1, missing // len(line.encode('utf-8')))

raw = json.dumps(base_body, ensure_ascii=False, separators=(',', ':')).encode('utf-8')
with open(request_path, 'wb') as f:
    f.write(raw)
print(json.dumps({'request_path': request_path, 'request_bytes': len(raw), 'model': model, 'provider_id': provider_id}, ensure_ascii=False))
PY

USEAI_CONTEXT_JSON="$(python3 - "$CONFIG_PATH" "$PROVIDER_ID" <<'PY'
import json
import sys
cfg = json.load(open(sys.argv[1], encoding='utf-8'))
provider_id = sys.argv[2].lower()
for p in cfg.get('providers', []):
    key = (p.get('id') or p.get('name') or '').lower()
    if key == provider_id or (p.get('name') or '').lower() == provider_id:
        print(json.dumps({'base_url': p.get('base_url','').rstrip('/'), 'api_key': p.get('api_key','')}, ensure_ascii=False))
        break
else:
    raise SystemExit('provider not found: ' + provider_id)
PY
)"
BASE_URL="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["base_url"])' "$USEAI_CONTEXT_JSON")"
API_KEY="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["api_key"])' "$USEAI_CONTEXT_JSON")"

printf '\n== 1. direct upstream: %s/chat/completions ==\n' "$BASE_URL"
DIRECT_STATUS="000"
set +e
DIRECT_BODY="$(curl -sS --max-time "$DIRECT_TIMEOUT_SECONDS" -w '\n__HTTP_STATUS__:%{http_code}\n__TIME_TOTAL__:%{time_total}\n' \
  -H 'Content-Type: application/json' \
  -H 'User-Agent: VS-AI-Proxy-Diagnostic/1.0' \
  -H "Authorization: Bearer $API_KEY" \
  --data-binary "@$REQUEST_PATH" \
  "$BASE_URL/chat/completions" 2>&1)"
DIRECT_RC=$?
set -e
printf '%s\n' "$DIRECT_BODY" > "$DIRECT_OUTPUT"
DIRECT_STATUS="$(printf '%s\n' "$DIRECT_BODY" | awk -F: '/__HTTP_STATUS__/{print $2}' | tail -1 | tr -d '\r')"
DIRECT_TIME_TOTAL="$(printf '%s\n' "$DIRECT_BODY" | awk -F: '/__TIME_TOTAL__/{print $2}' | tail -1 | tr -d '\r')"
printf 'direct rc=%s status=%s elapsed_ms=%s\n' "$DIRECT_RC" "${DIRECT_STATUS:-unknown}" "$(python3 - "$DIRECT_TIME_TOTAL" <<'PY'
import sys
try:
    print(f"{float(sys.argv[1]) * 1000:.0f}")
except Exception:
    print("unknown")
PY
)"
DIRECT_PREVIEW_LIMIT=800 DIRECT_PREVIEW_BODY="$DIRECT_BODY" python3 - <<'PY'
import os
limit = int(os.environ.get('DIRECT_PREVIEW_LIMIT', '800'))
body = '\n'.join(line for line in os.environ.get('DIRECT_PREVIEW_BODY', '').splitlines() if not line.startswith('__HTTP_STATUS__'))
print(body[:limit])
PY

printf '\n== 2. local proxy: build and start ==\n'
rtk go build -o "$PROXY_BIN" ./cmd/server
CONFIG_PATH="$CONFIG_PATH" STORE_PATH="$STORE_PATH" "$PROXY_BIN" >"$PROXY_LOG" 2>&1 &
echo $! > "$PID_FILE"
for _ in {1..80}; do
  if curl -sf "http://127.0.0.1:$PROXY_PORT/health" >/dev/null; then
    break
  fi
  sleep 0.25
done
curl -sf "http://127.0.0.1:$PROXY_PORT/health" >/dev/null

PROXY_REQUEST_PATH="$WORK_DIR/proxy-request.json"
python3 - "$REQUEST_PATH" "$PROXY_REQUEST_PATH" "$DISPLAY_PROVIDER" <<'PY'
import json
import sys
src, dst, display_provider = sys.argv[1:]
body = json.load(open(src, encoding='utf-8'))
body['model'] = f'{display_provider} - {body["model"]}'
with open(dst, 'wb') as f:
    f.write(json.dumps(body, ensure_ascii=False, separators=(',', ':')).encode('utf-8'))
PY

printf '\n== 3. proxy request: /v1/chat/completions ==\n'
PROXY_STATUS="000"
set +e
PROXY_BODY="$(curl -sS --max-time "$PROXY_TIMEOUT_SECONDS" -w '\n__HTTP_STATUS__:%{http_code}\n__TIME_TOTAL__:%{time_total}\n' \
  -H 'Content-Type: application/json' \
  --data-binary "@$PROXY_REQUEST_PATH" \
  "http://127.0.0.1:$PROXY_PORT/v1/chat/completions" 2>&1)"
PROXY_RC=$?
set -e
printf '%s\n' "$PROXY_BODY" > "$PROXY_OUTPUT"
PROXY_STATUS="$(printf '%s\n' "$PROXY_BODY" | awk -F: '/__HTTP_STATUS__/{print $2}' | tail -1 | tr -d '\r')"
PROXY_TIME_TOTAL="$(printf '%s\n' "$PROXY_BODY" | awk -F: '/__TIME_TOTAL__/{print $2}' | tail -1 | tr -d '\r')"
printf 'proxy rc=%s status=%s elapsed_ms=%s\n' "$PROXY_RC" "${PROXY_STATUS:-unknown}" "$(python3 - "$PROXY_TIME_TOTAL" <<'PY'
import sys
try:
    print(f"{float(sys.argv[1]) * 1000:.0f}")
except Exception:
    print("unknown")
PY
)"
PROXY_PREVIEW_LIMIT=1200 PROXY_PREVIEW_BODY="$PROXY_BODY" python3 - <<'PY'
import os
limit = int(os.environ.get('PROXY_PREVIEW_LIMIT', '1200'))
body = '\n'.join(line for line in os.environ.get('PROXY_PREVIEW_BODY', '').splitlines() if not line.startswith('__HTTP_STATUS__'))
print(body[:limit])
PY

stop_proxy

printf '\n== 4. proxy logs: request_bytes vs upstream_bytes ==\n'
python3 - "$CONFIG_PATH" "$RESULT_PATH" "$DIRECT_STATUS" "$PROXY_STATUS" "$DIRECT_RC" "$PROXY_RC" "$DIRECT_TIME_TOTAL" "$PROXY_TIME_TOTAL" <<'PY'
import json
import os
import sys
config_path, result_path, direct_status, proxy_status, direct_rc, proxy_rc, direct_time_total, proxy_time_total = sys.argv[1:]
def elapsed_ms(value):
    try:
        return round(float(value) * 1000, 3)
    except Exception:
        return None
log_path = os.path.join(os.path.dirname(config_path), 'logs.json')
logs = []
if os.path.exists(log_path):
    with open(log_path, encoding='utf-8') as f:
        logs = json.load(f)
chat_logs = [item for item in logs if item.get('method') == 'POST' and item.get('path') == '/v1/chat/completions']
last = chat_logs[-1] if chat_logs else (logs[-1] if logs else {})
result = {
    'direct_status': direct_status,
    'direct_rc': int(direct_rc),
    'direct_elapsed_ms': elapsed_ms(direct_time_total),
    'proxy_status': proxy_status,
    'proxy_rc': int(proxy_rc),
    'proxy_elapsed_ms': elapsed_ms(proxy_time_total),
    'last_proxy_log': last,
    'request_bytes': last.get('request_bytes'),
    'upstream_bytes': last.get('upstream_bytes'),
    'delta_bytes': (last.get('upstream_bytes') or 0) - (last.get('request_bytes') or 0),
}
print(json.dumps(result, ensure_ascii=False, indent=2))
with open(result_path, 'w', encoding='utf-8') as f:
    json.dump(result, f, ensure_ascii=False, indent=2)
PY

printf '\n诊断结果已保存: %s\n' "$RESULT_PATH"
