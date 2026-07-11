#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# 复用单次大请求诊断脚本，避免每个 provider/model 都临时写一遍测试逻辑。
# CASES 格式：provider_id|display_provider|model，每行一个；display_provider 必须匹配 VS 展示前缀。
CONFIG_SOURCE="${CONFIG_SOURCE:-$HOME/.config/vs-ai-proxy/config.json}"
CASES="${CASES:-deepseek|deepseek|deepseek-v4-flash
useai|UseAI|deepseek-v4-flash}"
SIZES="${SIZES:-50000 200000 500000 800000 1060000}"
DIRECT_TIMEOUT_SECONDS="${DIRECT_TIMEOUT_SECONDS:-120}"
PROXY_TIMEOUT_SECONDS="${PROXY_TIMEOUT_SECONDS:-140}"
INJECT_MODEL_BINDING="${INJECT_MODEL_BINDING:-0}"
PROXY_PORT_START="${PROXY_PORT_START:-12346}"
WORK_DIR="$ROOT_DIR/.bin/large-request-matrix"
JSONL_PATH="$WORK_DIR/results.jsonl"
SUMMARY_PATH="$WORK_DIR/summary.json"

mkdir -p "$WORK_DIR"
rm -f "$JSONL_PATH" "$SUMMARY_PATH"

case_index=0
while IFS='|' read -r provider_id display_provider model; do
  provider_id="$(printf '%s' "$provider_id" | xargs)"
  display_provider="$(printf '%s' "$display_provider" | xargs)"
  model="$(printf '%s' "$model" | xargs)"
  if [[ -z "$provider_id" || -z "$display_provider" || -z "$model" ]]; then
    continue
  fi

  for target_bytes in $SIZES; do
    port=$((PROXY_PORT_START + case_index))
    case_index=$((case_index + 1))
    safe_name="${provider_id}-${model}-${target_bytes}"
    safe_name="${safe_name//[^A-Za-z0-9_.-]/_}"
    result_copy="$WORK_DIR/${safe_name}.json"
    output_copy="$WORK_DIR/${safe_name}.out"

    printf '\n===== provider=%s display=%s model=%s target_bytes=%s port=%s =====\n' \
      "$provider_id" "$display_provider" "$model" "$target_bytes" "$port"

    set +e
    CONFIG_SOURCE="$CONFIG_SOURCE" \
      INJECT_MODEL_BINDING="$INJECT_MODEL_BINDING" \
      PROVIDER_ID="$provider_id" \
      DISPLAY_PROVIDER="$display_provider" \
      MODEL="$model" \
      TARGET_BYTES="$target_bytes" \
      PROXY_PORT="$port" \
      DIRECT_TIMEOUT_SECONDS="$DIRECT_TIMEOUT_SECONDS" \
      PROXY_TIMEOUT_SECONDS="$PROXY_TIMEOUT_SECONDS" \
      tests/useai_large_request_diagnostic.sh >"$output_copy" 2>&1
    rc=$?
    set -e

    tail -n 36 "$output_copy" || true

    if [[ -f .bin/useai-large-diagnostic/result.json ]]; then
      cp .bin/useai-large-diagnostic/result.json "$result_copy"
      python3 - "$result_copy" "$JSONL_PATH" "$provider_id" "$display_provider" "$model" "$target_bytes" "$rc" <<'PY'
import json
import sys
result_path, jsonl_path, provider_id, display_provider, model, target_bytes, rc = sys.argv[1:]
with open(result_path, encoding='utf-8') as f:
    result = json.load(f)
last = result.get('last_proxy_log') or {}
record = {
    'provider_id': provider_id,
    'display_provider': display_provider,
    'model': model,
    'target_bytes': int(target_bytes),
    'script_rc': int(rc),
    'direct_status': result.get('direct_status'),
    'direct_elapsed_ms': result.get('direct_elapsed_ms'),
    'proxy_status': result.get('proxy_status'),
    'proxy_elapsed_ms': result.get('proxy_elapsed_ms'),
    'error_code': last.get('error_code'),
    'request_bytes': result.get('request_bytes'),
    'upstream_bytes': result.get('upstream_bytes'),
    'delta_bytes': result.get('delta_bytes'),
    'elapsed_ms': last.get('elapsed_ms'),
    'response_tools': last.get('response_tools'),
}
with open(jsonl_path, 'a', encoding='utf-8') as f:
    f.write(json.dumps(record, ensure_ascii=False, separators=(',', ':')) + '\n')
print('SUMMARY', json.dumps(record, ensure_ascii=False))
PY
    else
      python3 - "$JSONL_PATH" "$provider_id" "$display_provider" "$model" "$target_bytes" "$rc" <<'PY'
import json
import sys
jsonl_path, provider_id, display_provider, model, target_bytes, rc = sys.argv[1:]
record = {
    'provider_id': provider_id,
    'display_provider': display_provider,
    'model': model,
    'target_bytes': int(target_bytes),
    'script_rc': int(rc),
    'error_code': 'diagnostic_script_failed',
}
with open(jsonl_path, 'a', encoding='utf-8') as f:
    f.write(json.dumps(record, ensure_ascii=False, separators=(',', ':')) + '\n')
print('SUMMARY', json.dumps(record, ensure_ascii=False))
PY
    fi
  done
done <<< "$CASES"

python3 - "$JSONL_PATH" "$SUMMARY_PATH" <<'PY'
import json
import sys
jsonl_path, summary_path = sys.argv[1:]
records = []
try:
    with open(jsonl_path, encoding='utf-8') as f:
        records = [json.loads(line) for line in f if line.strip()]
except FileNotFoundError:
    pass
with open(summary_path, 'w', encoding='utf-8') as f:
    json.dump(records, f, ensure_ascii=False, indent=2)
print('\n===== matrix summary =====')
for r in records:
    print(
        f"{r.get('provider_id')}\t{r.get('model')}\t{r.get('target_bytes')}\t"
        f"direct={r.get('direct_status')}\tproxy={r.get('proxy_status')}\t"
        f"code={r.get('error_code') or '-'}\treq={r.get('request_bytes')}\t"
        f"up={r.get('upstream_bytes')}\tdelta={r.get('delta_bytes')}\t"
        f"direct_ms={r.get('direct_elapsed_ms')}\tproxy_ms={r.get('proxy_elapsed_ms')}"
    )
print(f"\n矩阵结果: {summary_path}")
PY
