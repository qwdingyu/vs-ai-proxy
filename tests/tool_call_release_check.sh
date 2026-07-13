#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "== 1. Provider JSON/tool fidelity =="
go test ./internal/provider -count=1 -run 'Test.*Tool|Test.*FunctionCall|TestOpenAIProviderChatRawPreservesToolFields|TestToolCallPreservesUnknownNestedFields'

echo "== 2. Proxy tool normalization / aliases / DSML =="
go test ./internal/proxy -count=1 -run 'TestVisualStudioToolExecutionE2E|TestCopilotToolCatalog|TestKnownCopilot|TestCanonicalToolName|TestNormalize.*RunTests|TestProbeOpenAIStreamForDSML|TestStreamOpenAI.*Tool|TestStreamOpenAI.*Undeclared|TestParseOpenAIStreamPayloadConvertsLegacy|TestPayloadTooLargeHint|Test.*DSML|Test.*Dsml'

echo "== 3. Streaming business smoke =="
bash tests/streaming_test.sh
bash tests/streaming_ollama_test.sh

echo "TOOL_CALL_RELEASE_CHECK_OK"
