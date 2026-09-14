# Pointing clients at llmtap

Every client needs exactly one change: the base URL. Credentials, models and
behaviour stay as they were.

| Client | How |
| --- | --- |
| Codex CLI | Copy [`codex/config.toml`](codex/config.toml) into `~/.codex/config.toml` |
| Claude Code | `export ANTHROPIC_BASE_URL=http://localhost:8080` |
| OpenAI SDK (any language) | Set the client's base URL to `http://localhost:8080/v1` |
| curl | Replace `https://api.openai.com` with `http://localhost:8080` |

## Checking it works

```bash
curl -s http://localhost:8080/healthz

curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"count to five"}]}'
```

The second command should print chunks as they arrive, not all at once at the
end. If it pauses and then dumps everything, something in front of llmtap is
buffering — check for a proxy between you and it.
