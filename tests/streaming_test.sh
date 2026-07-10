#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

PROXY_BIN="$ROOT_DIR/.bin/vs-ai-proxy-streaming"
PYTHON_UPSTREAM="$ROOT_DIR/.bin/streaming_upstream.py"
CONFIG_PATH="$ROOT_DIR/.bin/streaming-config.json"
PROXY_PORT=11434
UPSTREAM_PORT=11435
PID_FILE="$ROOT_DIR/.bin/.streaming-test.pids"
OUTPUT_PATH="$ROOT_DIR/.bin/streaming-output.runtime.txt"

mkdir -p "$ROOT_DIR/.bin"

cat > "$PYTHON_UPSTREAM" <<'PY'
from http.server import HTTPServer, BaseHTTPRequestHandler
import json

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/v1/models':
            body = json.dumps({'data': [{'id': 'proxy-model'}]})
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(body.encode('utf-8'))
            return
        if self.path == '/api/tags':
            body = json.dumps({'models': [{'name': 'proxy-model'}]})
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.end_headers()
            self.wfile.write(body.encode('utf-8'))
            return
        self.send_response(200)
        self.end_headers()

    def do_POST(self):
        length = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(length).decode('utf-8') if length else ''
        data = json.loads(body) if body else {}
        model = data.get('model', 'test-model')

        self.send_response(200)
        self.send_header('Content-Type', 'application/x-ndjson')
        self.end_headers()

        chunks = [
            {"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": model,
             "choices": [{"index": 0, "delta": {"role": "assistant", "content": "Hello"}, "finish_reason": None}]},
            {"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": model,
             "choices": [{"index": 0, "delta": {"content": " world"}, "finish_reason": None}]},
            {"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": model,
             "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
             "usage": {"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}},
        ]

        for chunk in chunks:
            self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode('utf-8'))
            self.wfile.flush()
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def log_message(self, format, *args):
        return

server = HTTPServer(('127.0.0.1', 11435), Handler)
server.serve_forever()
PY

cleanup() {
  if [[ -f "$PID_FILE" ]]; then
    while read -r pid; do
      kill "$pid" >/dev/null 2>&1 || true
    done < "$PID_FILE"
    rm -f "$PID_FILE"
  fi
  rm -f "$PROXY_BIN" "$PYTHON_UPSTREAM" "$CONFIG_PATH" "$OUTPUT_PATH"
}
trap cleanup EXIT

cat > "$CONFIG_PATH" <<JSON
{
  "port": $PROXY_PORT,
  "default_model": "proxy-model",
  "providers": [
    {
      "name": "upstream",
      "api_key": "test-key",
      "base_url": "http://127.0.0.1:$UPSTREAM_PORT",
      "type": "openai",
      "enabled": true,
      "priority": 1
    }
  ],
  "models": [
    {
      "name": "proxy-model",
      "provider": "upstream",
      "context_length": 1024,
      "max_output_tokens": 256,
      "supports_tools": true,
      "enabled": true
    }
  ]
}
JSON

rtk go build -o "$PROXY_BIN" ./cmd/server
python3 "$PYTHON_UPSTREAM" &
UPSTREAM_PID=$!
echo "$UPSTREAM_PID" > "$PID_FILE"

for i in {1..40}; do
  if curl -sf "http://127.0.0.1:$UPSTREAM_PORT/health" >/dev/null; then
    break
  fi
  sleep 0.25
done

CONFIG_PATH="$CONFIG_PATH" "$PROXY_BIN" &
PROXY_PID=$!
echo "$PROXY_PID" >> "$PID_FILE"

for i in {1..40}; do
  if curl -sf "http://127.0.0.1:$PROXY_PORT/health" >/dev/null; then
    break
  fi
  sleep 0.25
done

curl -N -sS "http://127.0.0.1:$PROXY_PORT/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"proxy-model","messages":[{"role":"user","content":"hi"}],"stream":true}' \
  > "$OUTPUT_PATH" || true

if grep -q 'data: {"id":' "$OUTPUT_PATH"; then
  echo "STREAMING_OK"
else
  echo "STREAMING_FAIL"
  cat "$OUTPUT_PATH"
  exit 1
fi
