#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

echo "== 1. Provider / converter JSON and tool fidelity =="
go test ./internal/provider ./internal/converter -count=1 -run 'TestToolProtocolContract|Test.*Tool|Test.*FunctionCall|TestOpenAIProviderChatRawPreservesToolFields|TestToolCallPreservesUnknownNestedFields'

echo "== 2. Proxy tool normalization / aliases / DSML =="
go test ./internal/proxy -count=1 -run 'TestToolProtocolContract|TestVisualStudioToolExecutionE2E|TestCopilotToolCatalog|TestKnownCopilot|TestCanonicalToolName|TestNormalize.*RunTests|TestProbeOpenAIStreamForDSML|TestStreamOpenAI.*Tool|TestStreamOpenAI.*Undeclared|TestParseOpenAIStreamPayloadConvertsLegacy|TestPayloadTooLargeHint|Test.*DSML|Test.*Dsml'

echo "== 3. Streaming business smoke =="
bash tests/streaming_test.sh
bash tests/streaming_ollama_test.sh
rm -f .bin/logs.json

echo "TOOL_CALL_RELEASE_CHECK_OK"
