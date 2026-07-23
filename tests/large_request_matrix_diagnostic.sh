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
# REPEAT 控制每个 provider/model/size 组合重复执行次数。
# 真实上游间歇性故障不能靠单轮判断，发布前应用 20-30 轮以上观察成功率和错误分布。
REPEAT="${REPEAT:-1}"
DIRECT_TIMEOUT_SECONDS="${DIRECT_TIMEOUT_SECONDS:-120}"
PROXY_TIMEOUT_SECONDS="${PROXY_TIMEOUT_SECONDS:-140}"
INJECT_MODEL_BINDING="${INJECT_MODEL_BINDING:-0}"
MODE="${MODE:-direct-proxy}"
COOLDOWN_SECONDS="${COOLDOWN_SECONDS:-0}"
PROXY_PORT_START="${PROXY_PORT_START:-12346}"
WORK_DIR="$ROOT_DIR/.bin/large-request-matrix"
JSONL_PATH="$WORK_DIR/results.jsonl"
SUMMARY_PATH="$WORK_DIR/summary.json"

mkdir -p "$WORK_DIR"
rm -f "$JSONL_PATH" "$SUMMARY_PATH"

case_index=0
for round in $(seq 1 "$REPEAT"); do
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
      safe_name="${round}-${provider_id}-${model}-${target_bytes}"
      safe_name="${safe_name//[^A-Za-z0-9_.-]/_}"
      result_copy="$WORK_DIR/${safe_name}.json"
      output_copy="$WORK_DIR/${safe_name}.out"

      printf '\n===== round=%s/%s provider=%s display=%s model=%s target_bytes=%s port=%s =====\n' \
        "$round" "$REPEAT" "$provider_id" "$display_provider" "$model" "$target_bytes" "$port"

      # 防止单轮诊断在写 result.json 前异常退出时，矩阵脚本误读上一轮结果。
      rm -f .bin/useai-large-diagnostic/result.json

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
        MODE="$MODE" \
        COOLDOWN_SECONDS="$COOLDOWN_SECONDS" \
        tests/useai_large_request_diagnostic.sh >"$output_copy" 2>&1
      rc=$?
      set -e

      tail -n 36 "$output_copy" || true

      if [[ -f .bin/useai-large-diagnostic/result.json ]]; then
        cp .bin/useai-large-diagnostic/result.json "$result_copy"
        python3 - "$result_copy" "$JSONL_PATH" "$provider_id" "$display_provider" "$model" "$target_bytes" "$round" "$rc" <<'PY'
import json
import sys
result_path, jsonl_path, provider_id, display_provider, model, target_bytes, round_index, rc = sys.argv[1:]
with open(result_path, encoding='utf-8') as f:
    result = json.load(f)
last = result.get('last_proxy_log') or result.get('proxy_diagnostic') or {}
direct = result.get('direct') or {}
proxy = result.get('proxy') or {}
request_bytes = result.get('request_bytes') or last.get('request_bytes')
upstream_bytes = result.get('upstream_bytes') or last.get('upstream_bytes')
delta_bytes = None
if request_bytes is not None and upstream_bytes is not None:
    delta_bytes = int(upstream_bytes) - int(request_bytes)
record = {
    'round': int(round_index),
    'mode': result.get('mode'),
    'provider_id': provider_id,
    'display_provider': display_provider,
    'model': model,
    'target_bytes': int(target_bytes),
    'script_rc': int(rc),
    'direct_status': result.get('direct_status') or direct.get('status'),
    'direct_curl_rc': direct.get('curl_rc'),
    'direct_elapsed_ms': result.get('direct_elapsed_ms') or direct.get('total_ms'),
    'direct_sse_done': direct.get('sse_done'),
    'direct_error_kind': direct.get('error_kind'),
    'proxy_status': result.get('proxy_status') or proxy.get('status'),
    'proxy_curl_rc': proxy.get('curl_rc'),
    'proxy_elapsed_ms': result.get('proxy_elapsed_ms') or proxy.get('total_ms'),
    'proxy_sse_done': proxy.get('sse_done'),
    'proxy_error_kind': proxy.get('error_kind'),
    'error_code': last.get('error_code'),
    'request_bytes': request_bytes,
    'upstream_bytes': upstream_bytes,
    'delta_bytes': result.get('delta_bytes') if result.get('delta_bytes') is not None else delta_bytes,
    'elapsed_ms': last.get('elapsed_ms'),
    'stream_state': last.get('stream_state'),
    'network_peer': last.get('network_peer'),
    'attempts_summary': last.get('attempts_summary'),
    'diagnostic_summary': last.get('diagnostic_summary'),
    'cancel_reason': last.get('cancel_reason'),
    'fallback_mode': last.get('fallback_mode'),
    'effective_timeout_seconds': last.get('effective_timeout_seconds'),
    'response_tools': last.get('response_tools'),
}
with open(jsonl_path, 'a', encoding='utf-8') as f:
    f.write(json.dumps(record, ensure_ascii=False, separators=(',', ':')) + '\n')
print('SUMMARY', json.dumps(record, ensure_ascii=False))
PY
      else
        python3 - "$JSONL_PATH" "$provider_id" "$display_provider" "$model" "$target_bytes" "$round" "$rc" <<'PY'
import json
import sys
jsonl_path, provider_id, display_provider, model, target_bytes, round_index, rc = sys.argv[1:]
record = {
    'round': int(round_index),
    'mode': None,
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
done

python3 - "$JSONL_PATH" "$SUMMARY_PATH" <<'PY'
import json
import sys
from collections import Counter, defaultdict
jsonl_path, summary_path = sys.argv[1:]
records = []
try:
    with open(jsonl_path, encoding='utf-8') as f:
        records = [json.loads(line) for line in f if line.strip()]
except FileNotFoundError:
    pass

def is_success(record):
    # 成功率必须按运行模式判断：proxy-only 不要求 direct 成功，
    # direct-proxy 则要求两侧都完整成功，避免把上游直连失败误算成代理稳定。
    # 本脚本固定发送 stream=true；HTTP 2xx 但 curl 中断或缺 data:[DONE]
    # 代表半截流，不能算成功，否则发布前 soak 会虚报稳定性。
    def endpoint_ok(prefix):
        status = str(record.get(f'{prefix}_status') or '')
        return (
            status.startswith('2') and
            int(record.get(f'{prefix}_curl_rc') or 0) == 0 and
            record.get(f'{prefix}_error_kind') in (None, '') and
            record.get(f'{prefix}_sse_done') is True
        )
    mode = record.get('mode') or ''
    if mode == 'direct-only':
        return endpoint_ok('direct')
    if mode == 'proxy-only':
        return endpoint_ok('proxy')
    return endpoint_ok('direct') and endpoint_ok('proxy')

def percentile(values, pct):
    values = sorted(v for v in values if isinstance(v, (int, float)))
    if not values:
        return None
    index = int(round((len(values) - 1) * pct / 100))
    return values[index]

groups = defaultdict(list)
for record in records:
    key = (record.get('provider_id'), record.get('model'), record.get('target_bytes'))
    groups[key].append(record)

aggregate = []
for (provider_id, model, target_bytes), items in sorted(groups.items()):
    successes = [item for item in items if is_success(item)]
    proxy_elapsed = [item.get('proxy_elapsed_ms') for item in items]
    direct_elapsed = [item.get('direct_elapsed_ms') for item in items]
    aggregate.append({
        'provider_id': provider_id,
        'model': model,
        'target_bytes': target_bytes,
        'runs': len(items),
        'successes': len(successes),
        'success_rate': round(len(successes) / len(items), 4) if items else 0,
        'error_codes': dict(Counter(str(item.get('error_code') or '-') for item in items)),
        'stream_states': dict(Counter(str(item.get('stream_state') or '-') for item in items)),
        'direct_error_kinds': dict(Counter(str(item.get('direct_error_kind') or '-') for item in items)),
        'proxy_error_kinds': dict(Counter(str(item.get('proxy_error_kind') or '-') for item in items)),
        'network_peers': dict(Counter(str(item.get('network_peer') or '-') for item in items)),
        'proxy_elapsed_p50_ms': percentile(proxy_elapsed, 50),
        'proxy_elapsed_p95_ms': percentile(proxy_elapsed, 95),
        'direct_elapsed_p50_ms': percentile(direct_elapsed, 50),
        'direct_elapsed_p95_ms': percentile(direct_elapsed, 95),
        'sample_attempts_summary': next((item.get('attempts_summary') for item in items if item.get('attempts_summary')), None),
        'sample_diagnostic_summary': next((item.get('diagnostic_summary') for item in items if item.get('diagnostic_summary')), None),
    })

summary = {'records': records, 'aggregate': aggregate}
with open(summary_path, 'w', encoding='utf-8') as f:
    json.dump(summary, f, ensure_ascii=False, indent=2)
print('\n===== matrix summary =====')
for r in records:
    print(
        f"round={r.get('round')}\t{r.get('provider_id')}\t{r.get('model')}\t{r.get('target_bytes')}\t"
        f"direct={r.get('direct_status')}/rc={r.get('direct_curl_rc')}/done={r.get('direct_sse_done')}\t"
        f"proxy={r.get('proxy_status')}/rc={r.get('proxy_curl_rc')}/done={r.get('proxy_sse_done')}\t"
        f"code={r.get('error_code') or '-'}\tstate={r.get('stream_state') or '-'}\t"
        f"peer={r.get('network_peer') or '-'}\treq={r.get('request_bytes')}\t"
        f"up={r.get('upstream_bytes')}\tdelta={r.get('delta_bytes')}\t"
        f"direct_ms={r.get('direct_elapsed_ms')}\tproxy_ms={r.get('proxy_elapsed_ms')}"
    )
print('\n===== aggregate =====')
for item in aggregate:
    print(
        f"{item['provider_id']}\t{item['model']}\t{item['target_bytes']}\t"
        f"runs={item['runs']}\tsuccess_rate={item['success_rate']:.2%}\t"
        f"errors={item['error_codes']}\tstates={item['stream_states']}\t"
        f"direct_err={item['direct_error_kinds']}\tproxy_err={item['proxy_error_kinds']}\t"
        f"proxy_p50={item['proxy_elapsed_p50_ms']}\tproxy_p95={item['proxy_elapsed_p95_ms']}"
    )
print(f"\n矩阵结果: {summary_path}")
PY
