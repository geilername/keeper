// Lightroom: where published photos go. Reads GET /api/lightroom/status,
// offers "Connect Lightroom" (/auth/lightroom/login, which comes back to
// /?lightroom=connected|declined|failed), lists the user's collections and
// stores the choice with PUT /api/lightroom/target. This file is the whole
// client side of the feature.

const POLL_MS = 60000; // expiry and refresh are minutes apart

const $ = id => document.getElementById(id);

const RETURN_NOTE = {
  declined: 'Sign-in was cancelled at Adobe. Nothing changed.',
  failed: 'Adobe sign-in did not complete. The gateway log says why; try again.',
};

export function startLightroom() {
  const box = $('lr');
  const state = $('lr-state');
  const note = $('lr-note');
  const pick = $('lr-pick');
  const select = $('lr-target');
  const connectBtn = $('lr-connect');

  let status = null;
  let collections = null; // null: not read yet
  let reading = false;
  let saving = false;

  // Back from Adobe: say how it went once, then drop the parameter so a
  // reload does not say it again.
  const back = new URLSearchParams(location.search).get('lightroom');
  if (back) {
    if (RETURN_NOTE[back]) say(RETURN_NOTE[back]);
    const url = new URL(location.href);
    url.searchParams.delete('lightroom');
    history.replaceState(null, '', url.pathname + url.search + url.hash);
  }

  function say(text) {
    note.textContent = text || '';
    note.hidden = !text;
  }

  function time(iso) {
    const d = new Date(iso);
    const t = d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    if (d.toDateString() === new Date().toDateString()) return t;
    return d.toLocaleDateString([], { day: 'numeric', month: 'short' }) + ' ' + t;
  }

  // The state sentence: a state word in condensed caps, then plain words.
  function sentence(word, parts, alert) {
    state.replaceChildren();
    const b = document.createElement('b');
    b.textContent = word;
    state.append(b, ' ', ...parts);
    state.classList.toggle('alert', !!alert);
  }

  function mono(text) {
    const s = document.createElement('span');
    s.className = 'num';
    s.textContent = text;
    return s;
  }

  function render() {
    const s = status;
    box.hidden = !s;
    if (!s) return;
    const mockNote = s.mode === 'mock' ? ' The publisher still sends to the Lightroom mock (LIGHTROOM_MODE=mock).' : '';
    connectBtn.hidden = !(s.state === 'not_connected' || s.state === 'expired');
    pick.hidden = s.state !== 'connected';

    switch (s.state) {
      case 'not_configured':
        if (s.mode === 'mock') {
          sentence('Mock', ['Photos go to the Lightroom mock. Adobe Lightroom is not set up for this console.']);
        } else {
          sentence('Not set up', ['Set ADOBE_CLIENT_ID, ADOBE_CLIENT_SECRET and KEEPER_TOKEN_KEY on the gateway to connect Adobe Lightroom.'], true);
        }
        return;
      case 'not_connected':
        sentence('Not connected', ['Connect an Adobe account, then choose the collection photos go to.' + mockNote], s.mode === 'adobe');
        return;
      case 'expired':
        sentence('Sign-in expired', [`${s.account || 'The Adobe account'} needs to connect again. Photos wait until then.` + mockNote], true);
        return;
    }

    // connected
    const who = `as ${s.account || 'an Adobe account'} · token until `;
    if (s.target) {
      // The select beside this names the collection; no need to say it twice.
      sentence('Connected', [who, mono(time(s.expires_at)), '.' + mockNote]);
    } else {
      sentence('No collection', [`Connected ${who}`, mono(time(s.expires_at)), '. Photos wait until you choose a collection.' + mockNote], true);
    }
    renderSelect();
  }

  function renderSelect() {
    const current = status?.target?.album_id || '';
    const opts = [];
    const add = (value, label, disabled) => {
      const o = new Option(label, value, false, value === current);
      o.disabled = !!disabled;
      opts.push(o);
    };
    if (collections === null) {
      add('', reading ? 'Reading collections…' : 'Collections not read', true);
    } else {
      if (!current) add('', collections.length ? 'Choose a collection…' : 'No collections in this catalogue', true);
      for (const c of collections) add(c.id, c.path || c.name);
      if (current && !collections.some(c => c.id === current)) {
        add(current, `${status.target.path || status.target.name} (no longer in Lightroom)`, true);
      }
    }
    select.replaceChildren(...opts);
    select.value = current;
    select.disabled = saving || collections === null || collections.length === 0;
    select.setAttribute('aria-busy', String(saving || reading));
  }

  async function readStatus() {
    try {
      const res = await fetch('/api/lightroom/status');
      if (!res.ok) throw new Error(String(res.status));
      const next = await res.json();
      const nowConnected = next.state === 'connected';
      const wasConnected = status?.state === 'connected';
      status = next;
      if (nowConnected && (!wasConnected || collections === null)) readCollections();
      if (!nowConnected) collections = null;
    } catch {
      if (!status) return; // never loaded: keep the section hidden
      say('Could not read the Lightroom connection from the gateway. Showing the last known state.');
    }
    render();
  }

  async function readCollections() {
    if (reading) return;
    reading = true;
    renderSelect();
    try {
      const res = await fetch('/api/lightroom/collections');
      if (res.status === 409) { await readStatus(); return; }
      if (!res.ok) throw new Error((await res.text()).trim() || res.status);
      collections = await res.json();
    } catch (err) {
      say(`Could not read your collections (${err.message}). Choose again in a minute.`);
    } finally {
      reading = false;
      renderSelect();
    }
  }

  select.addEventListener('change', async () => {
    const id = select.value;
    if (!id || saving) return;
    saving = true;
    renderSelect();
    say('Saving…');
    try {
      const res = await fetch('/api/lightroom/target', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ album_id: id }),
      });
      if (!res.ok) throw new Error((await res.text()).trim() || res.status);
      status = { ...status, target: await res.json() };
      say('');
    } catch (err) {
      say(`The collection was not changed (${err.message}).`);
    }
    saving = false;
    render();
  });

  readStatus();
  setInterval(readStatus, POLL_MS);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) readStatus(); });
}
