// End-to-end check of the demo stack: every panel of the provisioned dashboard
// has to return data, the latency quantiles have to be plausible, and an
// exemplar has to lead to a trace that Tempo actually holds.
//
//   docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.demo.yml up -d --build
//   node deploy/smoke-test.js
//
// The queries are read from the dashboard JSON rather than copied in here, so a
// panel cannot drift away from what this test checks. Everything goes through
// Grafana's API, which also exercises datasource and dashboard provisioning.
//
// No dependencies: Node 18 or newer.
'use strict';

const fs = require('fs');
const path = require('path');

const GRAFANA = process.env.GRAFANA_URL || 'http://localhost:3000';
const TIMEOUT_MS = Number(process.env.SMOKE_TIMEOUT_MS || 180000);
const DASHBOARD_UID = 'llmtap-overview';
const WINDOW_MS = 5 * 60 * 1000;

// Plausibility limits for the demo traffic. The fake provider answers within
// about two seconds, and waits 120-900 ms before its first chunk. With the SDK
// default buckets p95 came out at a constant 4.75 s and p50 at 2.5 s; these
// limits are chosen to catch exactly that.
const LIMITS = {
  'Call duration': { max: 4 },
  'Time to first chunk': { max: 1.5, min: 0.05 },
};

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function getJSON(url, init) {
  const res = await fetch(url, init);
  if (!res.ok) throw new Error(`${init?.method || 'GET'} ${url}: HTTP ${res.status}`);
  return res.json();
}

// Retries a check until it passes or the overall deadline is reached. The
// stack needs a while after "up" before metrics have been pushed, scraped and
// become visible to increase() and rate().
//
// Every check is attempted at least once, even past the deadline: once one
// check has used up the time, the rest must still report their own result
// rather than a bare timeout.
async function eventually(name, deadline, fn) {
  for (;;) {
    try {
      const result = await fn();
      console.log(`ok    ${name}`);
      return result;
    } catch (err) {
      if (Date.now() + 5000 >= deadline) throw new Error(`${name}: ${err.message}`);
      await sleep(5000);
    }
  }
}

async function runQuery(target) {
  const now = Date.now();
  const expr = target.expr.replaceAll('$provider', '.*').replaceAll('$model', '.*');
  const body = {
    from: String(now - WINDOW_MS),
    to: String(now),
    queries: [{
      refId: target.refId,
      datasource: { uid: target.datasource.uid },
      expr,
      instant: Boolean(target.instant),
      range: !target.instant,
      exemplar: Boolean(target.exemplar),
      intervalMs: 15000,
      maxDataPoints: 100,
    }],
  };
  const res = await getJSON(`${GRAFANA}/api/ds/query`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });

  const result = res.results[target.refId];
  if (result.error) throw new Error(result.error);

  const frames = result.frames || [];
  const isExemplar = (f) => f.schema?.name === 'exemplar' || f.schema?.meta?.dataTopic === 'annotations';

  // The last non-null value of every series.
  const values = frames.filter((f) => !isExemplar(f)).map((f) => {
    const column = f.data.values.at(-1) || [];
    return column.filter((v) => v != null && !Number.isNaN(v)).at(-1);
  }).filter((v) => v !== undefined);

  const exemplarFrames = frames.filter(isExemplar);
  const traceIds = exemplarFrames.flatMap((f) => {
    const i = f.schema.fields.findIndex((field) => field.name === 'trace_id');
    return i < 0 ? [] : f.data.values[i];
  });

  return { values, traceIds };
}

async function main() {
  const deadline = Date.now() + TIMEOUT_MS;
  const file = path.join(__dirname, 'grafana', 'dashboards', 'llmtap-overview.json');
  const local = JSON.parse(fs.readFileSync(file, 'utf8'));

  await eventually('Grafana is up', deadline, () => getJSON(`${GRAFANA}/api/health`));

  const provisioned = await eventually('dashboard is provisioned', deadline, () =>
    getJSON(`${GRAFANA}/api/dashboards/uid/${DASHBOARD_UID}`));
  if (provisioned.dashboard.panels.length !== local.panels.length) {
    throw new Error(`Grafana has ${provisioned.dashboard.panels.length} panels, the file has ${local.panels.length}`);
  }

  const failures = [];
  const traceIds = new Set();

  for (const panel of local.panels) {
    for (const target of panel.targets) {
      const name = `panel "${panel.title}" ${target.refId}`;
      try {
        await eventually(name, deadline, async () => {
          const { values, traceIds: ids } = await runQuery(target);
          if (values.length === 0) throw new Error('no data');

          const limit = LIMITS[panel.title];
          if (limit) {
            for (const v of values) {
              if (v > limit.max) throw new Error(`value ${v} above ${limit.max}`);
              if (limit.min !== undefined && v < limit.min) throw new Error(`value ${v} below ${limit.min}`);
            }
          }
          if (target.exemplar && ids.length === 0) throw new Error('no exemplars');
          ids.forEach((id) => traceIds.add(id));
        });
      } catch (err) {
        console.log(`FAIL  ${err.message}`);
        failures.push(err.message);
      }
    }
  }

  // An exemplar is only useful if its trace exists. Tempo ingests with a
  // short delay, so a few IDs are tried until one resolves.
  try {
    await eventually('an exemplar resolves to a trace in Tempo', deadline, async () => {
      if (traceIds.size === 0) throw new Error('no exemplar trace IDs collected');
      for (const id of [...traceIds].slice(-10)) {
        const res = await fetch(`${GRAFANA}/api/datasources/proxy/uid/tempo/api/v2/traces/${id}`);
        if (res.ok) return;
      }
      throw new Error(`none of ${Math.min(traceIds.size, 10)} trace IDs found in Tempo`);
    });
  } catch (err) {
    console.log(`FAIL  ${err.message}`);
    failures.push(err.message);
  }

  if (failures.length > 0) {
    console.error(`\n${failures.length} check(s) failed.`);
    process.exit(1);
  }
  console.log('\nAll checks passed.');
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
