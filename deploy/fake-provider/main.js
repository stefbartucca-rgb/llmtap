// A stand-in for api.openai.com and api.anthropic.com.
//
// It speaks enough of both wire formats to exercise llmtap end to end -- it
// streams, reports token counts and cache hits, and fails occasionally -- so
// the stack can be tried out without an API key and without spending money.
//
// Never point anything real at this. It generates numbers, not answers.
const http = require('http');

const PORT = 19090;
const MODELS = {
  openai: ['gpt-5-codex', 'gpt-5.2-codex', 'gpt-5'],
  anthropic: ['claude-sonnet-4-6', 'claude-opus-5', 'claude-haiku-4-5'],
};

const pick = (a) => a[Math.floor(Math.random() * a.length)];
const rnd = (lo, hi) => Math.floor(lo + Math.random() * (hi - lo));

function openaiEvents(model, input, cached, output) {
  const id = 'resp_' + rnd(1e5, 9e5);
  return [
    ['response.created', { type: 'response.created', response: { id, model, status: 'in_progress' } }],
    ['response.output_text.delta', { type: 'response.output_text.delta', delta: '...' }],
    ['response.completed', {
      type: 'response.completed',
      response: {
        id, model, status: 'completed',
        usage: { input_tokens: input, input_tokens_details: { cached_tokens: cached }, output_tokens: output },
      },
    }],
  ];
}

// Chat Completions streams bare data lines and reports usage in one extra final
// chunk -- and only when the request asked for it via stream_options, which
// llmtap injects by default.
function chatEvents(model, input, cached, output, includeUsage) {
  const id = 'chatcmpl-' + rnd(1e5, 9e5);
  const chunk = (choices, usage) => ({ id, object: 'chat.completion.chunk', model, choices, usage });
  const events = [
    [null, chunk([{ index: 0, delta: { role: 'assistant', content: '...' }, finish_reason: null }], null)],
    [null, chunk([{ index: 0, delta: {}, finish_reason: 'stop' }], null)],
  ];
  if (includeUsage) {
    events.push([null, chunk([], {
      prompt_tokens: input,
      completion_tokens: output,
      total_tokens: input + output,
      prompt_tokens_details: { cached_tokens: cached },
    })]);
  }
  events.push([null, '[DONE]']);
  return events;
}

// Anthropic splits usage: input arrives up front, output is revised as it goes.
function anthropicEvents(model, input, cached, output) {
  const id = 'msg_' + rnd(1e5, 9e5);
  return [
    ['message_start', {
      type: 'message_start',
      message: { id, model, usage: { input_tokens: input, cache_read_input_tokens: cached, output_tokens: 1 } },
    }],
    ['content_block_delta', { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: '...' } }],
    ['message_delta', { type: 'message_delta', delta: { stop_reason: 'end_turn' }, usage: { output_tokens: output } }],
    ['message_stop', { type: 'message_stop' }],
  ];
}

http.createServer((req, res) => {
  let body = '';
  req.on('data', (c) => (body += c));
  req.on('end', () => {
    const isAnthropic = req.url.includes('/messages');
    const isChat = req.url.includes('/chat/completions');
    let requested = '';
    let includeUsage = false;
    try {
      const parsed = JSON.parse(body || '{}') || {};
      requested = parsed.model || '';
      includeUsage = Boolean(parsed.stream_options && parsed.stream_options.include_usage);
    } catch {
      // A malformed body is the client's problem, not a reason to stop.
    }
    const model = requested || pick(isAnthropic ? MODELS.anthropic : MODELS.openai);

    // Roughly one call in twenty is rejected, so error.type shows up in the
    // dashboard instead of staying theoretical.
    if (Math.random() < 0.05) {
      res.writeHead(429, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: { type: 'rate_limit_error', message: 'rate limit exceeded' } }));
      return;
    }

    // A wide spread of prompt sizes, with cache hits on most of them, which is
    // what agent traffic actually looks like.
    const input = rnd(800, 40000);
    const cached = Math.floor(input * Math.random() * 0.8);
    const output = rnd(50, 3000);

    let events;
    if (isAnthropic) events = anthropicEvents(model, input, cached, output);
    else if (isChat) events = chatEvents(model, input, cached, output, includeUsage);
    else events = openaiEvents(model, input, cached, output);

    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' });

    let i = 0;
    const firstChunkDelay = rnd(120, 900); // makes time_to_first_chunk meaningful
    const step = () => {
      if (i >= events.length) {
        res.end();
        return;
      }
      const [name, payload] = events[i++];
      const data = typeof payload === 'string' ? payload : JSON.stringify(payload);
      res.write(name ? `event: ${name}\ndata: ${data}\n\n` : `data: ${data}\n\n`);
      setTimeout(step, i === 1 ? firstChunkDelay : rnd(40, 250));
    };
    step();
  });
}).listen(PORT, '0.0.0.0', () => console.error(`fake provider listening on :${PORT}`));
