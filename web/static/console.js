// Keeper console: live exposure record of the shoot.
// Data: GET /api/photos (snapshot list) + SSE /api/events ("photo" events);
// search: GET /api/search (Elasticsearch) with the same snapshot shape.
// Rendering is keyed by sha256 and burst, so prints move instead of reloading.

import { startDemo } from './demo.js';
import { startLightroom } from './lightroom.js';

const BURST_GAP_MS = 2000;       // culler BURST_GAP, for bursts still forming
const WINDOW_IDLE_MS = 20000;    // culler BURST_IDLE
const WINDOW_MAX_MS = 90000;     // culler BURST_MAX_WAIT
const STALL_MS = 20000;          // a photo waiting this long for the scorer is news

const STAGE_WORD = {
  ingested: 'Waiting for scorer', scored: 'In burst window', decided: 'Decided',
  publishing: 'Publishing', retrying: 'Retrying', published: 'In Lightroom', failed: 'Not published',
};
const BUCKET_WORD = { best: 'Best', keeper: 'Keeper', reject: 'Reject' };

const photos = new Map();        // sha → snapshot
const fresh = new Set();         // shas that arrived live: develop them once
const printEls = new Map();      // sha → <button>
const burstEls = new Map();      // burst key → <section>
const found = new Map();         // sha → snapshot, from the last search
const foundEls = new Map();      // sha → <button> in the results
// The search on screen. active: a query or filter is set, and its results
// replace the bursts. seq drops answers to searches that were overtaken.
const search = { active: false, q: '', bucket: '', stage: '', shas: [], total: 0, error: null, busy: false, seq: 0 };
const lookup = sha => photos.get(sha) || found.get(sha);
let selected = null;
let userChose = false;
let connected = false;

const $ = id => document.getElementById(id);
const el = (tag, cls, text) => {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;
};
const svgUse = (id, cls) => {
  const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  if (cls) s.setAttribute('class', cls);
  s.setAttribute('aria-hidden', 'true');
  const u = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  u.setAttribute('href', '#' + id);
  s.append(u);
  return s;
};

// ── time and numbers ──────────────────────────────────────
const t = v => (v ? Date.parse(v) : NaN);
const captured = p => t(p.captured_at) || t(p.seen_at);
const arrived = p => t(p.seen_at) || captured(p);
const ago = ms => {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s} s`;
  const m = Math.round(s / 60);
  return m < 60 ? `${m} min` : `${Math.round(m / 60)} h`;
};
const secs = ms => (ms < 10000 ? (ms / 1000).toFixed(1) : Math.round(ms / 1000)) + ' s';
const clock = ms => {
  const d = new Date(ms);
  const pad = n => String(n).padStart(2, '0');
  // EXIF time has no zone: show it as the camera wrote it.
  return `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}.${Math.floor(d.getUTCMilliseconds() / 100)}`;
};
const plural = (n, one, many = one + 's') => `${n} ${n === 1 ? one : many}`;

// Exposure in stops from middle grey: the scorer's mean is gamma-encoded
// luminance (0..1); linearise and compare with 18 %.
const evOf = f => (f?.exposure ? Math.log2(Math.max(1e-4, f.exposure.mean ** 2.2) / 0.18) : null);
const ROMAN = ['0', 'I', 'II', 'III', 'IV', 'V', 'VI', 'VII', 'VIII', 'IX', 'X'];
const zoneOf = ev => ROMAN[Math.min(10, Math.max(0, Math.round(5 + ev)))];

// −1⅔ style, rounded to thirds, built as nodes (the fraction glyphs are not in the font subset)
function stops(ev, unit = '') {
  const frag = document.createDocumentFragment();
  const thirds = Math.round(ev * 3);
  const sign = thirds > 0 ? '+' : thirds < 0 ? '−' : '±';
  const whole = Math.floor(Math.abs(thirds) / 3);
  const rest = Math.abs(thirds) % 3;
  frag.append(sign + (whole || !rest ? whole : ''));
  if (rest) frag.append(el('span', 'frac', `${rest}⁄3`));
  if (unit) frag.append(' ' + unit);
  return frag;
}

// Next publish attempt, from the publisher's retry_at.
function nextTry(p, now = Date.now()) {
  const at = t(p.retry_at);
  if (!Number.isFinite(at)) return null;
  const left = at - now;
  return left > 500 ? `next try in ${ago(left)}` : 'trying now';
}

const signed = (v, digits) => {
  const r = Number(v.toFixed(digits)); // no "−0.00"
  return (r > 0 ? '+' : r < 0 ? '−' : '±') + Math.abs(r).toFixed(digits);
};

const hamming = (a, b) => {
  if (!a || !b || a.length !== b.length) return null;
  let n = 0;
  for (let i = 0; i < a.length; i += 4) {
    let x = parseInt(a.slice(i, i + 4), 16) ^ parseInt(b.slice(i, i + 4), 16);
    while (x) { n += x & 1; x >>= 1; }
  }
  return n;
};

// ── bursts ────────────────────────────────────────────────
function groupBursts(list) {
  const byId = new Map();
  const loose = [];
  for (const p of list) {
    if (p.burst_id) {
      if (!byId.has(p.burst_id)) byId.set(p.burst_id, []);
      byId.get(p.burst_id).push(p);
    } else loose.push(p);
  }
  const groups = [...byId].map(([id, ps]) => ({ key: 'b:' + id, decided: true, photos: ps }));
  loose.sort((a, b) => captured(a) - captured(b));
  let cur = null;
  for (const p of loose) {
    if (!cur || captured(p) - captured(cur.photos.at(-1)) > BURST_GAP_MS) {
      cur = { key: 'f:' + p.sha256, decided: false, photos: [] };
      groups.push(cur);
    }
    cur.photos.push(p);
  }
  for (const g of groups) {
    g.photos.sort((a, b) => captured(a) - captured(b) || a.sha256.localeCompare(b.sha256));
    g.first = captured(g.photos[0]);
    g.last = captured(g.photos.at(-1));
    g.name = commonName(g.photos.map(p => p.file_name || ''));
    g.best = g.photos.find(p => p.bucket === 'best') || null;
    // readings are given against one reference frame: the best, or while the
    // burst is still open, the sharpest frame so far
    g.ref = g.best || g.photos.filter(p => p.features)
      .reduce((m, p) => (!m || p.features.sharpness.score > m.features.sharpness.score ? p : m), null);
    g.arrivedFirst = Math.min(...g.photos.map(arrived));
    g.arrivedLast = Math.max(...g.photos.map(arrived));
  }
  // Order by arrival, not capture time: what came in last is what is live.
  groups.sort((a, b) => a.arrivedFirst - b.arrivedFirst).forEach((g, i) => { g.number = i + 1; });
  return groups.reverse(); // newest first
}

function commonName(names) {
  const stems = names.map(n => n.replace(/\.[^.]+$/, '')).filter(Boolean);
  if (!stems.length) return '';
  let pre = stems[0];
  for (const s of stems) while (!s.startsWith(pre)) pre = pre.slice(0, -1);
  return pre.replace(/[\s._-]*\d*$/, '');
}

const frameNo = (p, i) => (p.file_name?.match(/(\d+)(?:-dup)?\.[^.]+$/)?.[1] ?? String(i + 1).padStart(2, '0'));

function windowInfo(g, now) {
  const scored = g.photos.map(p => t(p.scored_at)).filter(Number.isFinite);
  if (!scored.length) return null;
  const closes = Math.min(Math.max(...scored) + WINDOW_IDLE_MS, Math.min(...scored) + WINDOW_MAX_MS);
  return { left: closes - now, idle: now - Math.max(...scored) };
}

// ── render ────────────────────────────────────────────────
// Move a node only when it is out of place: re-inserting a focused print
// every tick would drop keyboard focus.
function place(parent, node, index) {
  if (parent.children[index] !== node) parent.insertBefore(node, parent.children[index] || null);
}

let queued = false;
function schedule() {
  if (queued) return;
  queued = true;
  requestAnimationFrame(() => { queued = false; render(); });
}

function render() {
  const now = Date.now();
  const list = [...photos.values()];
  const groups = groupBursts(list);
  renderStages(list, groups, now);
  renderStatus(list, now);

  const root = $('bursts');
  root.classList.toggle('searching', search.active);
  $('empty').hidden = list.length > 0;
  const keep = new Set();
  groups.forEach((g, i) => {
    keep.add(g.key);
    let sec = burstEls.get(g.key);
    if (!sec) {
      sec = el('section', 'burst');
      sec.append(el('header', 'burst-head'), el('div', 'prints'));
      burstEls.set(g.key, sec);
    }
    sec.classList.toggle('older', i > 0);
    renderBurstHead(sec.firstChild, g, now);
    const prints = sec.lastChild;
    g.photos.forEach((p, j) => place(prints, printFor(p, j, g), j));
    place(root, sec, i + 2); // index 0 is the results, 1 the empty-state block
  });
  for (const [k, sec] of burstEls) if (!keep.has(k)) { sec.remove(); burstEls.delete(k); }

  renderFound(now);

  // Until the photographer picks a print, the record follows what is on
  // screen: the top search result, or the newest burst's best frame.
  if (!userChose || !lookup(selected)) {
    const g = groups[0];
    selected = search.active ? search.shas[0] ?? null : g ? (g.best || g.photos.at(-1)).sha256 : null;
  }
  markSelected();
  renderRecord(now);
  demoRender();
  fresh.clear();
}

function renderBurstHead(head, g, now) {
  head.replaceChildren();
  const h = el('h2', null, `Burst ${g.number}${g.name ? ' · ' + g.name : ''}`);
  const span = el('span', 'span',
    `${plural(g.photos.length, 'frame')} · ${clock(g.first)}${g.last !== g.first ? '–' + clock(g.last).slice(-4) : ''}`);
  const state = el('span', 'state');
  head.append(h, span, state);

  if (!g.decided) {
    const waiting = g.photos.filter(p => p.stage === 'ingested').length;
    const w = windowInfo(g, now);
    if (waiting) state.append(el('b', null, `Scoring ${g.photos.length - waiting} of ${g.photos.length}`));
    else if (w && w.left > 0) state.append('Window open · closes in about ', el('b', null, ago(w.left)));
    else state.append(el('b', null, 'Deciding'));
    if (w) {
      const bar = el('div', 'window');
      const fill = el('i');
      fill.style.transform = `scaleX(${Math.min(1, w.idle / WINDOW_IDLE_MS)})`;
      bar.append(fill);
      bar.setAttribute('aria-hidden', 'true');
      head.append(bar);
    }
    return;
  }
  const n = b => g.photos.filter(p => p.bucket === b).length;
  const retrying = g.photos.filter(p => p.stage === 'retrying').length;
  const failed = g.photos.filter(p => p.stage === 'failed').length;
  const published = g.photos.filter(p => p.stage === 'published').length;
  if (failed) state.append(el('b', null, `${failed} not published`), ' · ');
  if (retrying) state.append(el('b', null, `${retrying} retrying`), ' · ');
  state.append(`${n('best')} best, ${plural(n('keeper'), 'keeper')}, ${plural(n('reject'), 'reject')}`);
  if (!retrying && !failed) state.append(' · ', published === g.photos.length ? 'all in Lightroom' : `${published} of ${g.photos.length} in Lightroom`);
}

// The print element: the photo in its mask and an empty caption. One per
// photo and view (bursts, results), so each view keeps its own node.
function printShell(p, cache) {
  let b = cache.get(p.sha256);
  if (!b) {
    b = el('button', 'print');
    b.type = 'button';
    const frame = el('span', 'frame');
    const img = el('img');
    img.alt = '';
    img.loading = 'lazy';
    img.decoding = 'async';
    img.src = p.thumb_url;
    img.addEventListener('error', () => setTimeout(() => { img.src = p.thumb_url + '?r=' + Date.now(); }, 1500), { once: true });
    frame.append(img);
    b.append(frame, el('span', 'cap'));
    b.addEventListener('click', () => select(p.sha256, true));
    cache.set(p.sha256, b);
  }
  b.classList.toggle('reject', p.bucket === 'reject');
  return b;
}

function bucketStamp(p) {
  const bucket = el('span', 'bucket ' + (p.bucket || 'pending'));
  if (p.bucket) bucket.append(BUCKET_WORD[p.bucket] + ' ' + p.rating, svgUse('i-star'));
  else bucket.append(p.stage === 'ingested' ? 'Scoring' : 'Scored');
  return bucket;
}

// Where the photo is; a conflict is led by the cross-hatch mark.
function whereLine(p) {
  const where = el('span', 'where');
  if (p.stage === 'retrying') {
    where.classList.add('conflict');
    where.textContent = `Retrying, ${nextTry(p) || 'waiting'} · attempt ${p.attempts}`;
  } else if (p.stage === 'failed') {
    where.classList.add('conflict');
    where.textContent = 'Not published · Lightroom refused it';
  } else where.textContent = STAGE_WORD[p.stage] || p.stage;
  return where;
}

function printLabel(p) {
  return [p.file_name || p.sha256.slice(0, 12),
    p.bucket ? `${BUCKET_WORD[p.bucket]}, ${p.rating} stars` : STAGE_WORD[p.stage],
    p.stage === 'retrying' ? `retrying, attempt ${p.attempts}` : null].filter(Boolean).join(', ');
}

function printFor(p, i, g) {
  const b = printShell(p, printEls);
  if (fresh.has(p.sha256)) {
    b.classList.remove('fresh'); void b.offsetWidth; b.classList.add('fresh');
  }

  const cap = b.lastChild;
  cap.replaceChildren();
  const no = el('span', 'no', frameNo(p, i));
  const read = el('span', 'read');
  const f = p.features;
  const ref = g.ref;
  if (f && ref && ref !== p && ref.features) {
    read.append('Δ S ' + signed(f.sharpness.score - ref.features.sharpness.score, 2) + ' · Δ ');
    read.append(stops(evOf(f) - evOf(ref.features), 'EV'));
  } else if (f) {
    read.append('ref · S ' + f.sharpness.score.toFixed(2) + ' · ');
    read.append(stops(evOf(f), 'EV'));
  } else read.append('—');
  if (f?.eyes?.state === 'closed') read.append(' · eyes closed');
  cap.append(no, bucketStamp(p), read, whereLine(p));
  b.setAttribute('aria-label', printLabel(p));
  return b;
}

// A search result: outside its burst there is no reference frame for
// readings, so the caption gives the file name and the first reason
// instead; the record sheet has the readings.
function resultFor(p) {
  const b = printShell(p, foundEls);
  const cap = b.lastChild;
  cap.replaceChildren();
  const name = el('span', 'no name', p.file_name || p.sha256.slice(0, 12));
  const why = el('span', 'why', p.reasons?.[0] || 'Not decided yet');
  if (p.reasons?.length > 1) why.append(el('span', 'more', ` +${p.reasons.length - 1}`));
  cap.append(name, bucketStamp(p), why, whereLine(p));
  b.setAttribute('aria-label', printLabel(p));
  return b;
}

function renderStages(list, groups, now) {
  const by = s => list.filter(p => p.stage === s);
  const scoring = by('ingested');
  const windowed = by('scored');
  const publishing = list.filter(p => ['decided', 'publishing', 'retrying', 'failed'].includes(p.stage));
  const retrying = by('retrying');
  const failed = by('failed');
  const inLr = by('published');

  const set = (name, count, note, conflict) => {
    const li = document.querySelector(`.stage[data-stage="${name}"]`);
    li.querySelector('.count').textContent = count;
    li.querySelector('.note').textContent = note;
    const load = name === 'lightroom' ? 0 : count === 0 ? 0 : count <= 2 ? 1 : count <= 5 ? 2 : 3;
    li.dataset.load = load;
    if (conflict) li.dataset.conflict = ''; else delete li.dataset.conflict;
  };

  const oldest = scoring.length ? Math.max(...scoring.map(p => now - (t(p.ingested_at) || t(p.seen_at)))) : 0;
  set('scoring', scoring.length, scoring.length ? (oldest > STALL_MS ? `waiting ${ago(oldest)}` : 'being scored') : '');

  const forming = groups.filter(g => !g.decided).map(g => windowInfo(g, now)).filter(Boolean);
  const soonest = forming.length ? Math.min(...forming.map(w => w.left)) : null;
  set('window', windowed.length, soonest == null ? '' : soonest > 0 ? `decides in about ${ago(soonest)}` : 'deciding');

  let pubNote = '';
  if (failed.length) pubNote = `${failed.length} not published`;
  else if (retrying.length) {
    const soonest = retrying.map(p => t(p.retry_at)).filter(Number.isFinite).sort((a, b) => a - b)[0];
    pubNote = `${retrying.length} retrying` + (soonest ? ` · ${nextTry({ retry_at: new Date(soonest).toISOString() }, now)}` : '');
  }
  else if (publishing.length) pubNote = 'on the way';
  set('publishing', publishing.length, pubNote, retrying.length + failed.length > 0);

  const lastPub = inLr.length ? Math.max(...inLr.map(p => t(p.published_at) || 0)) : 0;
  set('lightroom', inLr.length, lastPub ? `last ${ago(now - lastPub)} ago` : '');
}

function renderStatus(list, now) {
  const s = $('status');
  s.replaceChildren();
  s.classList.remove('alert');
  const alert = (head, rest) => { s.classList.add('alert'); s.append(el('b', null, head), ' ' + rest); };
  if (!connected) { s.append('Reconnecting to Keeper…'); return; }
  if (!list.length) { s.append('Waiting for the first upload'); return; }
  const failed = list.filter(p => p.stage === 'failed').length;
  const retrying = list.filter(p => p.stage === 'retrying').length;
  const queued = list.filter(p => p.stage === 'ingested');
  const oldest = queued.length ? Math.max(...queued.map(p => now - (t(p.ingested_at) || t(p.seen_at)))) : 0;
  if (failed) return alert(`${plural(failed, 'photo')} not published.`, `Lightroom refused ${failed === 1 ? 'it' : 'them'}; the original${failed === 1 ? '' : 's'} stay${failed === 1 ? 's' : ''} in Keeper.`);
  if (retrying) return alert('Lightroom is not answering.', `${plural(retrying, 'photo')} retrying; nothing is lost.`);
  if (oldest > STALL_MS) return alert('Waiting for the scorer.', `${plural(queued.length, 'photo')} queued for ${ago(oldest)}.`);
  const last = Math.max(...list.map(p => t(p.seen_at) || 0));
  const idle = now - last > 120000;
  s.append(el('b', null, idle ? 'Idle' : 'Flowing'), ` · ${plural(list.length, 'photo')} · last arrival ${ago(now - last)} ago`);
}

// ── record sheet ──────────────────────────────────────────
function select(sha, byUser) {
  selected = sha;
  if (byUser) userChose = true;
  markSelected();
  renderRecord(Date.now());
  if (byUser && narrow.matches) {
    $('sheet').classList.add('open');
    $('close').focus();
  }
}

// The record follows the newest burst on wide screens; on phones nothing is
// marked until the photographer picks a print (the record is a panel there).
const narrow = matchMedia('(max-width: 1080px)');
function markSelected() {
  const show = userChose || !narrow.matches;
  for (const cache of [printEls, foundEls])
    for (const [s, b] of cache) b.setAttribute('aria-pressed', String(show && s === selected));
}

let shownSha = null;
function renderRecord(now) {
  const p = lookup(selected);
  $('record').hidden = !p;
  $('sheet-empty').hidden = !!p;
  if (!p) return;

  if (shownSha !== p.sha256) {
    const img = el('img');
    img.alt = p.file_name || '';
    img.src = p.thumb_url;
    $('r-print').replaceChildren(img);
    shownSha = p.sha256;
  }
  $('r-name').textContent = p.file_name || p.sha256;
  const groups = groupBursts([...photos.values()]);
  const g = groups.find(x => x.photos.some(q => q.sha256 === p.sha256));
  const idx = g ? g.photos.findIndex(q => q.sha256 === p.sha256) : 0;
  $('r-meta').textContent = [g ? `Burst ${g.number}, frame ${idx + 1} of ${g.photos.length}` : null,
    clock(captured(p))].filter(Boolean).join(' · ');

  const rows = $('r-rows');
  rows.replaceChildren();
  const row = (label, value, note) => {
    const tr = el('tr');
    const th = el('th', 'legend', label);
    th.scope = 'row';
    const td = el('td');
    td.append(value);
    if (note) td.append(el('small', null, note));
    tr.append(th, td);
    rows.append(tr);
  };
  if (p.bucket) {
    const v = document.createDocumentFragment();
    const st = el('span', 'stars');
    for (let i = 0; i < p.rating; i++) st.append(svgUse('i-star'));
    v.append(st, ' ' + BUCKET_WORD[p.bucket]);
    row('Rating', v, p.bucket === 'best' ? 'best frame of its burst' : null);
  } else row('Rating', 'pending', 'rated when the burst closes');
  row('Stage', STAGE_WORD[p.stage] || p.stage,
    p.stage === 'retrying' || p.stage === 'failed'
      ? [p.stage === 'retrying' ? nextTry(p, now) : null, `attempt ${p.attempts}`, p.last_error].filter(Boolean).join(' · ')
      : null);

  const f = p.features;
  if (f) {
    const best = g?.photos.reduce((m, q) => Math.max(m, q.features?.sharpness.score ?? 0), 0) || 0;
    row('Sharpness', f.sharpness.score.toFixed(2),
      best && f.sharpness.score >= best ? 'sharpest in burst' : best ? `${Math.round(100 * f.sharpness.score / best)} % of the burst's sharpest (${best.toFixed(2)})` : null);
    const ev = evOf(f);
    const exp = document.createDocumentFragment();
    exp.append(`Zone ${zoneOf(ev)} · `, stops(ev, 'EV'));
    row('Exposure', exp, `mean ${f.exposure.mean.toFixed(2)} · clipped ${(100 * (f.exposure.highlights_clipped + f.exposure.shadows_clipped)).toFixed(1)} %`);
    row('Eyes', { open: 'open', closed: 'closed', no_face: 'no face' }[f.eyes.state] || f.eyes.state,
      f.eyes.faces ? `${plural(f.eyes.faces, 'face')} · confidence ${f.eyes.confidence.toFixed(2)}` : null);
    const ref = g?.ref;
    if (ref && ref.sha256 !== p.sha256 && ref.features) {
      const d = hamming(f.dhash, ref.features.dhash);
      const dev = document.createDocumentFragment();
      dev.append('Δ ', stops(ev - evOf(ref.features), 'EV'));
      row('Against ref', dev, `${ref.file_name || 'reference frame'}${d != null ? ` · ${d} bits apart` : ''}`);
    }
  }

  const reasons = $('r-reasons');
  reasons.replaceChildren();
  if (p.reasons?.length) p.reasons.forEach(r => reasons.append(el('li', null, r)));
  else reasons.append(el('li', null, 'Not decided yet. Reasons appear when the burst closes.'));

  renderJourney(p, now);

  const a = $('r-trace');
  if (p.trace_url) { a.href = p.trace_url; a.removeAttribute('aria-disabled'); a.removeAttribute('tabindex'); }
  else { a.removeAttribute('href'); a.setAttribute('aria-disabled', 'true'); a.tabIndex = -1; }
  // Without a configured Dash0 link the id alone is still useful: it can be
  // pasted into Dash0's trace search.
  const tid = $('r-trace-id');
  tid.replaceChildren('trace ', el('span', 'hex', p.trace_id));
}

function renderJourney(p, now) {
  const marks = [
    ['Ingest', arrived(p), t(p.ingested_at)],
    ['Scoring', t(p.ingested_at), t(p.scored_at)],
    ['Burst window', t(p.scored_at), t(p.decided_at)],
    ['Publishing', t(p.decided_at), t(p.published_at)],
  ];
  const segs = [];
  for (const [label, from, to] of marks) {
    if (!Number.isFinite(from)) break;
    const done = Number.isFinite(to);
    segs.push({ label, ms: Math.max(0, (done ? to : now) - from), done });
    if (!done) break;
  }
  const total = segs.reduce((s, x) => s + x.ms, 0);
  $('r-total').textContent = total ? secs(total) : '';
  const box = $('r-journey');
  box.replaceChildren();
  const bar = el('div', 'bar');
  bar.setAttribute('aria-hidden', 'true');
  const ol = el('ol');
  for (const s of segs) {
    const i = el('i');
    i.style.flexGrow = Math.max(s.ms, total * 0.02);
    if (!s.done) i.className = p.stage === 'retrying' || p.stage === 'failed' ? 'conflict' : 'wait';
    bar.append(i);
    const li = el('li');
    li.append(el('span', null, s.label + (s.done ? '' : ' (now)')), el('span', null, secs(s.ms)));
    ol.append(li);
  }
  box.append(bar, ol);
}

// ── search ────────────────────────────────────────────────
const BUCKET_FILTER = { best: 'best frames', keeper: 'keepers', reject: 'rejects' };

function renderFound(now) {
  const box = $('found');
  box.hidden = !search.active;
  if (!search.active) return;

  const n = search.shas.length;
  const title = $('found-title');
  const note = $('found-note');
  note.hidden = true;
  note.classList.remove('alert');
  if (search.error) {
    title.textContent = search.error.head;
    note.textContent = search.error.text;
    note.classList.add('alert');
    note.hidden = false;
  } else if (search.busy && !n) {
    title.textContent = 'Searching…';
  } else if (!n) {
    title.textContent = 'No photos match';
    note.textContent = search.q
      ? 'Words match the reasons Keeper gives, like blur or "eyes closed". Check the spelling, or set rating and stage back to any.'
      : 'No photo has this rating and stage right now.';
    note.hidden = false;
  } else {
    title.textContent = n < search.total
      ? `${n} of ${plural(search.total, 'photo')} found, best matches first`
      : `${plural(n, 'photo')} found`;
  }

  const forWhat = $('found-for');
  forWhat.replaceChildren();
  const parts = [];
  if (search.q) { const f = document.createDocumentFragment(); f.append('for ', el('code', null, search.q)); parts.push(f); }
  if (search.bucket) parts.push(BUCKET_FILTER[search.bucket]);
  if (search.stage) parts.push((STAGE_WORD[search.stage] || search.stage).toLowerCase());
  parts.forEach((x, i) => forWhat.append(...(i ? [' · ', x] : [x])));

  const grid = $('found-prints');
  const keep = new Set();
  search.shas.forEach((sha, i) => {
    const p = lookup(sha);
    if (!p) return;
    keep.add(sha);
    place(grid, resultFor(p), i);
  });
  for (const [sha, b] of foundEls) if (!keep.has(sha)) { b.remove(); foundEls.delete(sha); }
}

function readForm() {
  search.q = $('find-q').value.trim();
  search.bucket = $('find-bucket').value;
  search.stage = $('find-stage').value;
  search.active = Boolean(search.q || search.bucket || search.stage);
  // Keep the search in the address, so a reload or a shared link shows it.
  const url = new URL(location.href);
  for (const k of ['q', 'bucket', 'stage']) {
    if (search[k]) url.searchParams.set(k, search[k]); else url.searchParams.delete(k);
  }
  history.replaceState(null, '', url.pathname + url.search + url.hash);
}

// runSearch asks the gateway; quiet is a background refresh, which keeps
// the results on screen instead of saying "Searching…".
async function runSearch({ quiet = false } = {}) {
  // A refresh that overtakes the photographer's pending search answers for
  // it, errors included.
  if (search.busy) quiet = false;
  const seq = ++search.seq;
  if (!search.active) {
    Object.assign(search, { shas: [], total: 0, error: null, busy: false });
    found.clear();
    schedule();
    return;
  }
  if (!quiet) {
    // A new search: the old results must not stand under the new query.
    Object.assign(search, { busy: true, error: null, shas: [], total: 0 });
    found.clear();
    schedule();
  }
  const params = new URLSearchParams({ limit: '100' });
  for (const k of ['q', 'bucket', 'stage']) if (search[k]) params.set(k, search[k]);
  let error = null, data = null;
  try {
    const res = await fetch('/api/search?' + params);
    if (res.ok) data = await res.json();
    else {
      const text = (await res.text()).trim().replace(/^bad query:\s*/, '');
      error = res.status === 400
        ? { head: 'Keeper could not read that search', text: `${text}. Words in quotes match a phrase; a field search looks like stage:retrying.` }
        : { head: 'Search is not available right now', text: 'The search index is not answering. The live flow is not affected; clear the search to return to it.' };
    }
  } catch {
    error = { head: 'Search is not available right now', text: 'Keeper did not answer. Check the connection; the search runs again on the next update.' };
  }
  if (seq !== search.seq) return; // a newer search is on its way
  search.busy = false;
  if (error) {
    if (!quiet) { search.error = error; search.shas = []; found.clear(); }
  } else {
    search.error = null;
    found.clear();
    for (const p of data.photos) found.set(p.sha256, p);
    search.shas = data.photos.map(p => p.sha256);
    search.total = data.total;
  }
  schedule();
}

// New events can change who matches; ask again once the index has caught
// up (2.5 s after the last event). During a shoot events never pause that
// long, so a refresh is also due at most 6 s after the first unrefreshed one.
const REFRESH_QUIET_MS = 2500;
const REFRESH_MAX_MS = 6000;
let refreshTimer = 0;
let refreshDue = 0;
function refreshSearch() {
  const now = Date.now();
  if (!refreshDue) refreshDue = now + REFRESH_MAX_MS;
  clearTimeout(refreshTimer);
  refreshTimer = setTimeout(() => {
    refreshDue = 0;
    runSearch({ quiet: true });
  }, Math.max(0, Math.min(REFRESH_QUIET_MS, refreshDue - now)));
}

function submitSearch() {
  readForm();
  userChose = false; // the record follows the top result
  runSearch();
}

async function startSearch() {
  // Without Elasticsearch configured the gateway answers 503 with "not
  // configured": then there is no search, and no line for it.
  try {
    const res = await fetch('/api/search?limit=1');
    if (res.status === 503 && /not configured/.test(await res.text())) return;
  } catch { /* the gateway is restarting: show the line anyway */ }

  const form = $('find');
  const q = $('find-q');
  const params = new URLSearchParams(location.search);
  q.value = params.get('q') || '';
  $('find-bucket').value = params.get('bucket') || '';
  $('find-stage').value = params.get('stage') || '';
  form.hidden = false;

  form.addEventListener('submit', e => { e.preventDefault(); submitSearch(); });
  for (const id of ['find-bucket', 'find-stage']) $(id).addEventListener('change', submitSearch);
  // Emptying the field (typing, or the field's own clear) returns to the flow.
  q.addEventListener('input', () => { if (!q.value && search.q) submitSearch(); });
  q.addEventListener('keydown', e => {
    if (e.key === 'Escape' && q.value) { e.stopPropagation(); q.value = ''; submitSearch(); }
  });
  $('find-try').addEventListener('click', e => {
    const b = e.target.closest('button[data-q]');
    if (!b) return;
    q.value = b.dataset.q;
    submitSearch();
  });
  $('found-clear').addEventListener('click', () => {
    form.reset();
    submitSearch();
    q.focus();
  });
  // "/" jumps to the search field from anywhere but another field.
  document.addEventListener('keydown', e => {
    if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
    if (e.target.closest?.('input, select, textarea, [contenteditable]')) return;
    e.preventDefault();
    q.focus();
    q.select();
  });

  readForm();
  if (search.active) runSearch();
}

// ── data ──────────────────────────────────────────────────
async function load() {
  try {
    const res = await fetch('/api/photos?limit=200');
    if (!res.ok) throw new Error(res.status);
    const list = await res.json();
    photos.clear();
    for (const p of list) photos.set(p.sha256, p);
  } catch (err) {
    console.warn('photo list failed', err);
  }
  schedule();
}

function connect() {
  const es = new EventSource('/api/events');
  es.onopen = () => { connected = true; load(); };
  es.onerror = () => { connected = false; schedule(); };
  es.addEventListener('photo', e => {
    const p = JSON.parse(e.data);
    if (!photos.has(p.sha256)) fresh.add(p.sha256);
    photos.set(p.sha256, { ...photos.get(p.sha256), ...p });
    if (found.has(p.sha256)) found.set(p.sha256, { ...found.get(p.sha256), ...p });
    if (search.active) refreshSearch();
    schedule();
  });
}

$('close').addEventListener('click', () => $('sheet').classList.remove('open'));
document.addEventListener('keydown', e => { if (e.key === 'Escape') $('sheet').classList.remove('open'); });
// Demo fault panel: compares the Lightroom mock's asset count with Keeper's.
const demoRender = startDemo(() => {
  const list = [...photos.values()];
  return {
    published: list.filter(p => p.stage === 'published').length,
    onTheWay: list.filter(p => ['decided', 'publishing', 'retrying'].includes(p.stage)).length,
  };
});

// Where published photos go: Adobe connection and target collection.
startLightroom();

// Search over every photo the index holds, not only the last 200.
startSearch();

setInterval(schedule, 1000); // countdowns and "ago" lines
connect();
