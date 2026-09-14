// Drives a steady trickle of calls through llmtap so the dashboards have
// something to show. Part of the demo overlay only.
const http = require('http');

const TARGET = process.env.LLMTAP_URL || 'http://llmtap:8080';
const EVERY_MS = Number(process.env.INTERVAL_MS || 1500);

const ROUTES = [
  { path: '/v1/responses', body: () => ({ model: pick(['gpt-5-codex', 'gpt-5.2-codex']), stream: true, input: 'demo' }) },
  { path: '/v1/chat/completions', body: () => ({ model: 'gpt-4.1', stream: true, messages: [{ role: 'user', content: 'demo' }] }) },
  { path: '/v1/messages', body: () => ({ model: pick(['claude-sonnet-4-6', 'claude-haiku-4-5']), stream: true, max_tokens: 1024, messages: [{ role: 'user', content: 'demo' }] }) },
];

const pick = (a) => a[Math.floor(Math.random() * a.length)];

function fire() {
  const route = pick(ROUTES);
  const payload = JSON.stringify(route.body());
  const url = new URL(TARGET + route.path);

  const req = http.request({
    hostname: url.hostname,
    port: url.port || 80,
    path: url.pathname,
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      // Not a real credential. It is here so the proxy exercises the same
      // header path it would in production.
      'Authorization': 'Bearer demo-not-a-real-key',
      'x-api-key': 'demo-not-a-real-key',
      'anthropic-version': '2023-06-01',
    },
  }, (res) => {
    res.resume(); // drain, so the stream completes and the span is closed
    res.on('end', () => {});
  });

  req.on('error', (err) => console.error('demo traffic:', err.message));
  req.end(payload);
}

console.error(`demo traffic -> ${TARGET} every ${EVERY_MS}ms`);
setInterval(fire, EVERY_MS);
fire();
