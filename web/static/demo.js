// Demo controls: break and heal the Lightroom mock from the console.
// The gateway proxies /api/demo/* to lightroom-mock's /admin API only when it
// runs with DEMO_CONTROLS=1; otherwise the first read is a 404 and the panel
// stays hidden. This file is the whole client side of the feature.

const SLOW_MS = 3000;     // "Latency 3 s"
const POLL_MS = 2000;     // stats refresh while the panel is open and visible

const $ = id => document.getElementById(id);

// keeper() returns the console's own counts: { published, onTheWay }.
export function startDemo(keeper) {
  const panel = $('demo');
  const toggles = { error: $('f-503'), latency: $('f-slow') };
  const heal = $('f-heal');
  let faults = null;       // last config the mock reported
  let stats = null;
  let busy = false;
  let lost = '';           // the last poll failed: said until a poll succeeds
  let refused = '';        // the last change failed: said for a few seconds
  let refusedUntil = 0;

  // Open by default on a laptop; on a phone it would push the photos a screen
  // down, so it starts closed there (the summary still says the fault state).
  let saved = null;
  try { saved = localStorage.getItem('keeper.demo.open'); } catch { /* storage off */ }
  panel.open = saved ? saved === '1' : !matchMedia('(max-width: 640px)').matches;
  // Remember only the presenter's own choice (a click or Enter on the
  // summary, which fires before the panel toggles), not the default above.
  panel.querySelector('summary').addEventListener('click', () => {
    try { localStorage.setItem('keeper.demo.open', panel.open ? '0' : '1'); } catch { /* storage off */ }
  });
  panel.addEventListener('toggle', () => { if (panel.open) poll(); });

  async function call(method, path, body) {
    const res = await fetch('/api/demo/' + path, {
      method,
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
    if (res.status === 404) return null; // demo controls switched off
    if (!res.ok) throw new Error(`${res.status} ${(await res.text()).trim()}`);
    return res.json();
  }

  // The mock replaces its whole config on every POST, so each change sends
  // all three fields, built from the state it last reported.
  async function set(next) {
    if (busy) return;
    busy = true;
    render();
    try {
      faults = await call('POST', 'faults', next);
      refusedUntil = 0;
    } catch (err) {
      refused = `Could not change the Lightroom mock (${err.message}). Its faults are as shown.`;
      refusedUntil = Date.now() + 8000;
    }
    busy = false;
    await poll();
  }

  const current = () => ({
    error_rate: faults?.error_rate ?? 0,
    status: 503,
    latency_ms: faults?.latency_ms ?? 0,
  });

  toggles.error.addEventListener('click', () => {
    set({ ...current(), error_rate: faults?.error_rate > 0 ? 0 : 1 });
  });
  toggles.latency.addEventListener('click', () => {
    set({ ...current(), latency_ms: faults?.latency_ms > 0 ? 0 : SLOW_MS });
  });
  heal.addEventListener('click', () => {
    if (heal.getAttribute('aria-disabled') !== 'true') set({ error_rate: 0, status: 503, latency_ms: 0 });
  });

  async function poll() {
    try {
      const s = await call('GET', 'stats');
      if (s === null) { stop(); return; }
      stats = s;
      if (!busy) faults = s.faults; // a change in flight wins over an older read
      lost = '';
      panel.hidden = false;
    } catch (err) {
      lost = `Lost contact with the Lightroom mock (${err.message}). Trying again every ${POLL_MS / 1000} s.`;
    }
    render();
  }

  function render() {
    const err = faults?.error_rate ?? 0;
    const lat = faults?.latency_ms ?? 0;
    for (const [key, on] of [['error', err > 0], ['latency', lat > 0]]) {
      const b = toggles[key];
      b.setAttribute('aria-pressed', String(on));
      b.lastChild.textContent = on ? 'On' : 'Off';
      b.setAttribute('aria-busy', String(busy));
    }
    heal.setAttribute('aria-disabled', String(busy || (!err && !lat)));

    const state = $('demo-state');
    state.replaceChildren();
    if (!faults) state.append('Reading the Lightroom mock…');
    else if (!err && !lat) state.append('Lightroom mock healthy');
    else {
      const parts = [];
      if (err >= 1) parts.push(`answers ${faults.status} to every request`);
      else if (err > 0) parts.push(`answers ${faults.status} to ${Math.round(err * 100)} % of requests`);
      if (lat) parts.push(`adds ${lat / 1000} s to each one`);
      const b = document.createElement('b');
      b.textContent = 'Fault on:';
      state.append(b, ' Lightroom mock ' + parts.join(' and '));
    }

    const k = keeper();
    $('d-assets').textContent = stats ? stats.assets : '–';
    $('d-published').textContent = k.published;
    $('d-replays').textContent = stats ? stats.create_replays : '–';
    $('d-injected').textContent = stats ? stats.faults_injected : '–';

    // The verdict is a live region: rewrite it only when the words change,
    // or screen readers repeat it every second.
    const verdict = $('demo-verdict');
    const text = verdictText(k);
    if (verdict.textContent !== text) verdict.textContent = text;
  }

  function verdictText(k) {
    const problem = lost || (Date.now() < refusedUntil ? refused : '');
    if (problem) return problem;
    if (!stats) return '';
    if (k.onTheWay) return `${k.onTheWay} on the way to Lightroom. The counts settle once they arrive.`;
    if (stats.assets === k.published) {
      return k.published ? `Exactly once: ${k.published} published by Keeper, ${stats.assets} in Lightroom, none doubled.` : 'Nothing published yet.';
    }
    return 'Counts differ: the mock keeps its catalogue in memory and empties it on restart, and the console counts its newest 200 photos.';
  }

  let timer = null;
  function stop() { panel.hidden = true; clearInterval(timer); timer = null; }

  poll();
  timer = setInterval(() => { if (panel.open && !document.hidden) poll(); }, POLL_MS);
  // Keeper's own counts change between polls; keep the comparison current.
  return () => { if (!panel.hidden && !busy) render(); };
}
