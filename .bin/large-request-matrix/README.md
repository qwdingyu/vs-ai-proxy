# Large Request Matrix Evidence

This directory stores sanitized diagnostic evidence for the v0.2.66 stability review.

Files:

- `*-*.json`: one sanitized diagnostic result per matrix run.
- `*-*.out`: command output captured next to each run.
- `results.jsonl`: compact line-delimited records regenerated from the JSON samples.
- `summary.json`: aggregate grouped by provider/model/request size/mode.

Important boundaries:

- API keys, Authorization headers, request bodies, and response bodies are not stored here.
- The payload size fields are byte counts only; the prompt/tool text is intentionally absent.
- `1-missing-missing-model-1.out` is retained as stale-result regression evidence for the diagnostic script. It has no JSON result and is intentionally excluded from `results.jsonl` and `summary.json`.
- These files are release evidence, not runtime input. Do not make proxy behavior depend on them.
