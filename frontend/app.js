// GraveStack PWA. One article a day, rendered in-app. No list, no menu.

let current = null;       // current article object
let readStarted = false;  // fired the read_started event yet?
let maxScroll = 0;        // deepest scroll %, for the completed signal
let completedFired = false;

const $ = (id) => document.getElementById(id);
const show = (el) => el.classList.remove('hidden');
const hide = (el) => el.classList.add('hidden');

// The Anthropic key lives only in this browser (localStorage) and rides along as
// a header so the server can generate pitches without ever persisting it.
const KEY_STORE = 'gs_anthropic_key';
function anthropicKey() { return localStorage.getItem(KEY_STORE) || ''; }

async function api(path, opts = {}) {
  const headers = Object.assign({ 'Content-Type': 'application/json' }, opts.headers || {});
  const k = anthropicKey();
  if (k) headers['X-Anthropic-Key'] = k;
  const res = await fetch(path, Object.assign({}, opts, { headers }));
  if (res.status === 401) { showLogin(); throw new Error('unauthorized'); }
  return res;
}

// ---------- boot ----------
async function boot() {
  registerSW();
  const res = await fetch('/api/session');
  const s = await res.json();
  if (s.needsLogin && !s.authed) { showLogin(); return; }
  loadToday();
}

function showLogin() {
  hide($('reader')); hide($('settings')); show($('login'));
}

$('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const res = await fetch('/api/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: $('password').value }),
  });
  if (res.ok) { hide($('login')); loadToday(); }
  else show($('login-error'));
});

// ---------- today's article ----------
async function loadToday() {
  show($('reader'));
  hide($('login')); hide($('settings'));
  hide($('dismissed')); hide($('empty'));
  const res = await api('/api/today');
  const data = await res.json();
  if (data.empty) { hideArticle(); show($('empty')); return; }
  if (data.dismissed) { hideArticle(); show($('dismissed')); return; }
  renderArticle(data.article, data.canReroll);
}

function hideArticle() { hide($('article')); }

function renderArticle(a, canReroll) {
  current = a;
  readStarted = false; maxScroll = 0; completedFired = false;
  show($('article'));

  const cover = $('cover');
  if (a.cover_image_url) { cover.style.backgroundImage = `url("${a.cover_image_url}")`; cover.classList.remove('empty'); }
  else cover.classList.add('empty');

  $('title').textContent = a.title || '';
  $('pitch-line').textContent = a.pitch_line || a.subtitle || '';
  $('pull-quote').textContent = a.pull_quote ? '“' + a.pull_quote + '”' : '';

  const parts = [];
  if (a.author) parts.push(a.author);
  if (a.word_count) parts.push(readingTime(a.word_count));
  if (a.is_paywalled) parts.push('preview only');
  $('meta').textContent = parts.join('  ·  ');

  $('body').innerHTML = a.body_html || '';

  // Reroll appears only when the server allows it (REROLLS_PER_DAY > 0).
  const nt = $('not-today');
  if (canReroll) { nt.textContent = 'Not this one → reroll'; nt.onclick = doReroll; }
  else { nt.textContent = 'Not today →'; nt.onclick = doNotToday; }

  window.scrollTo(0, 0);
}

function readingTime(words) {
  const m = Math.max(1, Math.round(words / 220));
  return m + ' min read';
}

// ---------- events (scroll depth → v2 brain fuel) ----------
window.addEventListener('scroll', throttle(() => {
  if (!current || $('article').classList.contains('hidden')) return;
  const h = document.documentElement;
  const pct = Math.min(100, Math.round(((h.scrollTop + window.innerHeight) / h.scrollHeight) * 100));
  if (pct > maxScroll) maxScroll = pct;
  if (!readStarted && pct > 15) { readStarted = true; sendEvent('read_started', pct); }
  if (!completedFired && pct >= 90) { completedFired = true; sendEvent('completed', pct); }
}, 800));

function sendEvent(type, pct) {
  if (!current) return;
  api('/api/events', { method: 'POST', body: JSON.stringify({ article_id: current.id, type, scroll_pct: pct || 0 }) }).catch(() => {});
}
// Log an abandon signal when leaving mid-read.
window.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'hidden' && current && readStarted && !completedFired) {
    navigator.sendBeacon('/api/events', JSON.stringify({ article_id: current.id, type: 'abandoned', scroll_pct: maxScroll }));
  }
});

// ---------- reroll / not today ----------
async function doReroll(e) {
  if (e) e.preventDefault();
  const res = await api('/api/reroll', { method: 'POST' });
  if (res.status === 403) { $('not-today').textContent = 'No reroll left today'; return; }
  const data = await res.json();
  if (data.empty) { hideArticle(); show($('empty')); return; }
  renderArticle(data.article, data.canReroll);
}

async function doNotToday(e) {
  if (e) e.preventDefault();
  await api('/api/not-today', { method: 'POST' });
  hideArticle(); show($('dismissed'));
}

// ---------- settings ----------
function openSettings() {
  hide($('reader')); show($('settings'));
  loadSettings();
}
function closeSettings() { hide($('settings')); loadToday(); }
$('gear').addEventListener('click', openSettings);

async function loadSettings() {
  const res = await api('/api/settings');
  const s = await res.json();
  $('notify_time').value = s.notify_time || '';
  $('timezone').value = s.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || '';
  $('saved_list_url').value = s.saved_list_url || '';
  $('anthropic_key').value = anthropicKey();
  $('key-status').textContent = anthropicKey() ? 'Key set in this browser.' : 'No key yet — pitches fall back to the subtitle.';
  $('sync-status').textContent = s.hasCookie ? 'Cookie saved. Sync anytime.' : '';
}

// saveKey stores the Anthropic key in this browser only (never sent to the
// settings endpoint). It rides along as a header on future requests.
function saveKey() {
  const v = $('anthropic_key').value.trim();
  if (v) localStorage.setItem(KEY_STORE, v);
  else localStorage.removeItem(KEY_STORE);
  $('key-status').textContent = v ? 'Saved in this browser.' : 'Cleared.';
}

async function saveSettings() {
  await api('/api/settings', { method: 'POST', body: JSON.stringify({
    notify_time: $('notify_time').value,
    timezone: $('timezone').value,
    saved_list_url: $('saved_list_url').value,
    cookie: $('cookie').value,
  })});
  $('sync-status').textContent = 'Saved.';
}

async function doSync() {
  $('sync-status').textContent = 'Syncing…';
  const res = await api('/api/sync', { method: 'POST', body: JSON.stringify({ cookie: $('cookie').value }) });
  const data = await res.json();
  reportSync(res.ok, data);
}
async function doSyncJSON() {
  $('sync-status').textContent = 'Importing…';
  const res = await api('/api/sync', { method: 'POST', body: JSON.stringify({ savedJson: $('saved-json').value }) });
  const data = await res.json();
  reportSync(res.ok, data);
}
function reportSync(ok, data) {
  if (!ok) { $('sync-status').textContent = 'Error: ' + (data.error || 'sync failed'); return; }
  $('sync-status').textContent = `Added ${data.new}, skipped ${data.skipped}, failed ${data.failed}. Pitches generating…`;
  loadLibrary();
}

// ---------- boring library (escape hatch) ----------
async function loadLibrary() {
  const res = await api('/api/library');
  const items = await res.json();
  const box = $('library');
  box.innerHTML = '';
  if (!items || !items.length) { box.textContent = 'Empty.'; return; }
  let lastTopic = null;
  for (const it of items) {
    const topic = it.topic || 'Uncategorized';
    if (topic !== lastTopic) {
      const h = document.createElement('div'); h.className = 'lib-topic'; h.textContent = topic;
      box.appendChild(h); lastTopic = topic;
    }
    const d = document.createElement('div'); d.className = 'lib-item';
    d.innerHTML = `${escapeHTML(it.title)}<div class="lib-meta">${escapeHTML(it.author || '')}${it.word_count ? ' · ' + readingTime(it.word_count) : ''}</div>`;
    d.onclick = () => openFromLibrary(it.id);
    box.appendChild(d);
  }
}
// Opening from the library shows it in the reader but does NOT change today's pick.
async function openFromLibrary(id) {
  const res = await api('/api/article/' + id);
  const data = await res.json();
  hide($('settings')); show($('reader'));
  hide($('dismissed')); hide($('empty'));
  renderArticle(data.article, false);
}

// ---------- notifications ----------
async function enableNotifications() {
  try {
    const perm = await Notification.requestPermission();
    if (perm !== 'granted') { $('notify-status').textContent = 'Permission denied.'; return; }
    const reg = await navigator.serviceWorker.ready;
    const keyRes = await api('/api/vapid-public-key');
    const { key } = await keyRes.json();
    const sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(key),
    });
    await api('/api/subscribe', { method: 'POST', body: JSON.stringify(sub) });
    $('notify-status').textContent = 'Notifications on for this device.';
  } catch (e) {
    $('notify-status').textContent = 'Could not enable: ' + e.message;
  }
}

// ---------- utils ----------
function throttle(fn, ms) { let last = 0, t; return (...a) => {
  const now = Date.now();
  if (now - last >= ms) { last = now; fn(...a); }
  else { clearTimeout(t); t = setTimeout(() => { last = Date.now(); fn(...a); }, ms - (now - last)); }
}; }
function escapeHTML(s) { const d = document.createElement('div'); d.textContent = s; return d.innerHTML; }
function urlBase64ToUint8Array(base64String) {
  const padding = '='.repeat((4 - base64String.length % 4) % 4);
  const base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(base64);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}
function registerSW() {
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});
}

boot();
