#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

PROXY_BIN="$ROOT_DIR/.bin/vs-ai-proxy-streaming-ollama"
PYTHON_UPSTREAM="$ROOT_DIR/.bin/streaming_ollama_upstream.py"
CONFIG_PATH="$ROOT_DIR/.bin/streaming-ollama-config.json"
PROXY_PORT=11436
UPSTREAM_PORT=11437
PID_FILE="$ROOT_DIR/.bin/.streaming-ollama-test.pids"
OUTPUT_PATH="$ROOT_DIR/.bin/streaming-ollama-output.runtime.txt"

mkdir -p "$ROOT_DIR/.bin"

cat > "$PYTHON_UPSTREAM" <<'PY'
from http.server import HTTPServer, BaseHTTPRequestHandler
import json

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/api/tags':
            body = json.dumps({'models': [{'name': 'ollama-model'}]})
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
        model = data.get('model', 'ollama-model')

        self.send_response(200)
        self.send_header('Content-Type', 'application/x-ndjson')
        self.end_headers()

        chunks = [
            {"model": model, "message": {"role": "assistant", "content": "Ollama"}, "done": False},
            {"model": model, "message": {"role": "assistant", "content": "Ollama stream"}, "done": False},
            {"model": model, "message": {"role": "assistant", "content": "Ollama stream!"}, "done": True,
             "prompt_eval_count": 5, "eval_count": 2},
        ]

        for chunk in chunks:
            self.wfile.write((json.dumps(chunk) + "\n").encode('utf-8'))
            self.wfile.flush()

    def log_message(self, format, *args):
        return

server = HTTPServer(('127.0.0.1', 11437), Handler)
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
  "default_model": "ollama-model",
  "providers": [
    {
      "name": "ollama-upstream",
      "api_key": "",
      "base_url": "http://127.0.0.1:$UPSTREAM_PORT",
      "type": "ollama",
      "enabled": true,
      "priority": 1
    }
  ],
  "models": [
    {
      "name": "ollama-model",
      "provider": "ollama-upstream",
      "context_length": 4096,
      "max_output_tokens": 1024,
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

ROOT_DIR="$ROOT_DIR" python3 - <<'PYCLIENT'
import urllib.request
import json
import os

req = urllib.request.Request(
    'http://127.0.0.1:11436/v1/chat/completions',
    data=json.dumps({"model":"ollama-model","messages":[{"role":"user","content":"hi"}],"stream":True}).encode('utf-8'),
    headers={'Content-Type': 'application/json'},
    method='POST'
)
with urllib.request.urlopen(req, timeout=10) as resp:
    body = resp.read().decode('utf-8')

with open(os.path.join(os.environ['ROOT_DIR'], '.bin', 'streaming-ollama-output.runtime.txt'), 'w') as f:
    f.write(body)

print(body)
PYCLIENT

if grep -q '^data: {' "$OUTPUT_PATH" && grep -q '^data: \[DONE\]' "$OUTPUT_PATH"; then
  echo "STREAMING_OLLAMA_OK"
else
  echo "STREAMING_OLLAMA_FAIL"
  cat "$OUTPUT_PATH"
  exit 1
fi
