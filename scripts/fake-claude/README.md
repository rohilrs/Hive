# fake-claude

Test substitute for the real `claude -p` binary. Reads a fixture JSONL file
and emits each line to stdout (with optional `delay_ms` between events).

## Usage

```bash
fake-claude -fixture fixtures/approve_immediately.jsonl
fake-claude --plugin-dir IGNORED --mcp-config IGNORED -p "ignored prompt"
HIVE_FAKE_CLAUDE_FIXTURE=fixtures/changes_requested_once.jsonl fake-claude
```

## Fixtures

- `approve_immediately.jsonl` — one implement message + immediate
  `hive_submit_review_verdict(APPROVE)` tool call.
- `changes_requested_once.jsonl` — first iteration returns CHANGES_REQUESTED.
- `no_verdict_tool.jsonl` — emits an implement message and exits without
  calling the verdict tool (triggers Haiku-classifier fallback path).
- `tool_call_stall.jsonl` — emits a `tool_call` event then sleeps; tests
  L2 stall detection.

## Fixture format

Each line is a JSON object representing one event. An optional `delay_ms`
field (consumed and removed) controls the pause before the next event.

```json
{"type": "text", "delta": "Looking at the failing test...", "delay_ms": 500}
{"type": "tool_use", "name": "hive_submit_review_verdict",
 "input": {"verdict": "APPROVE", "confidence": 92, "issues": []}}
```
