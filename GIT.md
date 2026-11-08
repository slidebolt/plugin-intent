# Git Workflow for plugin-intent

This repository contains the Slidebolt Intent Plugin. Each intent is a button whose `Phrase` label lists the ways of saying it; automations on the button do the work. A listener device per channel (text, phone) takes requests: a confident phrase match presses the button at once, anything else goes to an OpenCode agent when one is configured. Every request is recorded for tuning. It produces a standalone binary.

## Dependencies
- **Internal:** `sb-contract`, `sb-domain`, `sb-logging-sdk`, `sb-messenger-sdk`, `sb-runtime`, `sb-storage-sdk`, `sb-testkit`.
- **Runtime (optional):** `plugin-opencode` agents for requests no phrase matches.

## Configuration
- `INTENT_LISTENERS`: `text:agent_text,phone:agent_phone`. Each listener names its fallback agent; leave it empty (`text:`) for none.
- `INTENT_MIN_SCORE` (default 0.8) and `INTENT_MIN_MARGIN` (default 0.15): the two gates a phrase match must pass.
- `INTENT_LLM_TIMEOUT_SECONDS` (default 300).
- `INTENT_LOG_DIR` (default `.data/intent`): monthly `requests-YYYY-MM.jsonl` files, one line per request.

## Build Process
- **Type:** Go Application (Plugin).
- **Command:** `go build -o plugin-intent ./cmd/plugin-intent`
- **Validation:** `go test ./...`. Phrase-matching accuracy is benchmarked in `sb-e2e/bench/intent`.
