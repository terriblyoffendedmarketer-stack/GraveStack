// GraveStack v2 — curated home surface + in-app reader.

let current = null;
let readStarted = false;
let maxScroll = 0;
let completedFired = false;

const $ = (id) => document.getElementById(id);
const show = (el) => el.classList.remove('hidden');
const hide = (el) => el.classList.add('hidden');

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
  loadHome();
}

function showLogin() {
  hideAll(); show($('login'));
}

function hideAll() {
  for (const id of ['login', 'home', 'reader', 'thread-view', 'magazine', 'issue-view', 'settings']) hide($(id));
}

$('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const res = await fetch('/api/login', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ password: $('password').value }),
  });
  if (res.ok) { hide($('login')); loadHome(); }
  else show($('login-error'));
});

// ---------- home ----------
async function loadHome() {
  hideAll(); show($('home'));
  hide($('empty'));

  const res = await api('/api/home');
  const data = await res.json();
  if (data.empty) { show($('empty')); return; }

  renderFeatured(data.featured);
  renderSuggestions(data.suggestions || []);
  renderWriteup(data.writeup);
  renderThreadsNav(data.threads || []);
  loadIssues();
}

function renderFeatured(f) {
  if (!f || !f.article) return;
  const a = f.article;
  const cover = $('featured-cover');
  if (a.cover_image_url) {
    cover.style.backgroundImage = `url("${a.cover_image_url}")`;
    cover.classList.remove('generated');
  } else {
    cover.style.backgroundImage = generatedCover(a.title || 'untitled');
    cover.classList.add('generated');
  }

  $('featured-thread').textContent = f.thread || '';
  $('featured-title').textContent = a.title || '';
  $('featured-context').textContent = a.pitch_line || a.subtitle || f.context || '';

  // Show first ~200 chars of plain text as a reading preview.
  const preview = stripToPreview(a.body_html, 200);
  $('featured-preview').textContent = preview;

  const parts = [];
  if (a.author) parts.push(a.author);
  if (f.read_time) parts.push(f.read_time + ' min read');
  else if (a.word_count) parts.push(readingTime(a.word_count));
  $('featured-meta').textContent = parts.join('  ·  ');

  $('featured').onclick = () => openArticle(a.id);
}

function renderSuggestions(suggestions) {
  const box = $('suggestion-cards');
  box.innerHTML = '';
  if (!suggestions.length) { hide($('suggestions')); return; }
  show($('suggestions'));

  for (const s of suggestions) {
    const a = s.article;
    const card = document.createElement('div');
    card.className = 'suggestion-card';
    card.onclick = () => openArticle(a.id);

    let imgHTML = '';
    if (a.cover_image_url) {
      imgHTML = `<div class="suggestion-card-img" style="background-image:url('${escapeAttr(a.cover_image_url)}')"></div>`;
    }

    const parts = [];
    if (a.author) parts.push(a.author);
    if (s.read_time) parts.push(s.read_time + ' min');
    else if (a.word_count) parts.push(readingTime(a.word_count));

    card.innerHTML = `${imgHTML}
      <div class="suggestion-card-body">
        <div class="suggestion-card-thread">${escapeHTML(s.thread || '')}</div>
        <div class="suggestion-card-title">${escapeHTML(a.title)}</div>
        <div class="suggestion-card-reason">${escapeHTML(s.reason || s.context || '')}</div>
        <div class="suggestion-card-meta">${escapeHTML(parts.join(' · '))}</div>
      </div>`;
    box.appendChild(card);
  }
}

function renderWriteup(text) {
  if (!text) { hide($('writeup-section')); return; }
  show($('writeup-section'));
  $('writeup').textContent = text;
}

function renderThreadsNav(threads) {
  if (!threads || !threads.length) { hide($('threads-section')); return; }
  show($('threads-section'));
  const box = $('threads-list');
  box.innerHTML = '';
  for (const t of threads) {
    const chip = document.createElement('div');
    chip.className = 'thread-chip';
    chip.innerHTML = `<span>${escapeHTML(t.title)}</span><span class="thread-chip-count">${t.article_count}</span>`;
    chip.onclick = () => openThread(t.slug);
    box.appendChild(chip);
  }
}

// ---------- reader ----------
async function openArticle(id) {
  hideAll(); show($('reader'));
  hide($('related'));
  window.scrollTo(0, 0);

  const res = await api('/api/article/' + id);
  const data = await res.json();
  renderArticle(data.article);
  loadRelated(id);
}

function renderArticle(a) {
  current = a;
  readStarted = false; maxScroll = 0; completedFired = false;

  const cover = $('cover');
  if (a.cover_image_url) {
    cover.style.backgroundImage = `url("${a.cover_image_url}")`;
    cover.classList.remove('empty', 'generated');
  } else {
    cover.style.backgroundImage = generatedCover(a.title || 'untitled');
    cover.classList.remove('empty');
    cover.classList.add('generated');
  }

  $('title').textContent = a.title || '';
  $('pitch-line').textContent = a.pitch_line || a.subtitle || '';
  $('pull-quote').textContent = a.pull_quote ? '“' + a.pull_quote + '”' : '';

  const parts = [];
  if (a.author) parts.push(a.author);
  if (a.word_count) parts.push(readingTime(a.word_count));
  if (a.is_paywalled) parts.push('preview only');
  $('meta').textContent = parts.join('  ·  ');

  $('body').innerHTML = a.body_html || '';
}

async function loadRelated(id) {
  try {
    const res = await api('/api/article/' + id + '/related');
    const items = await res.json();
    if (!items || !items.length) return;
    show($('related'));
    const box = $('related-cards');
    box.innerHTML = '';
    for (const r of items.slice(0, 3)) {
      const a = r.article;
      const card = document.createElement('div');
      card.className = 'suggestion-card';
      card.onclick = () => openArticle(a.id);

      const parts = [];
      if (a.author) parts.push(a.author);
      if (a.word_count) parts.push(readingTime(a.word_count));

      card.innerHTML = `<div class="suggestion-card-body">
        <div class="suggestion-card-title">${escapeHTML(a.title)}</div>
        <div class="suggestion-card-reason">${escapeHTML(r.reason || r.relation || '')}</div>
        <div class="suggestion-card-meta">${escapeHTML(parts.join(' · '))}</div>
      </div>`;
      box.appendChild(card);
    }
  } catch (e) {}
}

function backToHome() {
  loadHome();
}

// ---------- thread view ----------
async function openThread(slug) {
  hideAll(); show($('thread-view'));
  window.scrollTo(0, 0);

  const res = await api('/api/thread/' + slug);
  const data = await res.json();
  $('thread-title').textContent = data.thread.title;
  $('thread-desc').textContent = data.thread.description;

  const box = $('thread-articles');
  box.innerHTML = '';
  for (const item of (data.articles || [])) {
    const a = item.article;
    const el = document.createElement('div');
    el.className = 'thread-article';
    el.onclick = () => openArticle(a.id);

    const parts = [];
    if (a.author) parts.push(a.author);
    if (a.word_count) parts.push(readingTime(a.word_count));

    el.innerHTML = `<div class="thread-article-title">${escapeHTML(a.title)}</div>
      <div class="thread-article-context">${escapeHTML(item.thread_context || '')}</div>
      <div class="thread-article-meta">${escapeHTML(parts.join(' · '))}</div>`;
    box.appendChild(el);
  }
}

// ---------- AI ask ----------
$('ask-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const q = $('ask-input').value.trim();
  if (!q) return;

  const resultBox = $('ask-result');
  show(resultBox);
  resultBox.innerHTML = '<div class="loading">Thinking</div>';

  try {
    const res = await api('/api/ask', { method: 'POST', body: JSON.stringify({ query: q }) });
    const data = await res.json();
    const arts = data.articles || {};

    let html = '';

    // Hero cover from main pick.
    const mainArt = arts[String(data.main_pick)];
    if (mainArt && mainArt.cover_image_url) {
      html += `<div class="ask-hero" style="background-image:url('${escapeAttr(mainArt.cover_image_url)}')" onclick="openArticle(${mainArt.id})"></div>`;
    } else if (mainArt) {
      html += `<div class="ask-hero ask-hero-gen" style="background-image:${generatedCover(mainArt.title || '')}" onclick="openArticle(${mainArt.id})"></div>`;
    }

    // Writeup with inline article links.
    if (data.writeup) {
      let writeupHTML = escapeHTML(data.writeup);
      for (const [id, a] of Object.entries(arts)) {
        const titleRe = new RegExp(escapeRegex(a.title), 'gi');
        const link = `<a class="ask-article-link" href="#" onclick="event.preventDefault();openArticle(${a.id})">${escapeHTML(a.title)}</a>`;
        writeupHTML = writeupHTML.replace(titleRe, link);
      }
      // Split into paragraphs.
      writeupHTML = writeupHTML.split(/\n\n+/).map(p => `<p>${p}</p>`).join('');
      html += `<div class="ask-writeup">${writeupHTML}</div>`;
    }

    // Inline article cards woven after the writeup.
    const picks = [data.main_pick, ...(data.supporting || [])].filter(Boolean);
    if (picks.length) {
      html += '<div class="ask-picks">';
      for (const pid of picks) {
        const a = arts[String(pid)];
        if (!a) continue;
        let coverHTML = '';
        if (a.cover_image_url) {
          coverHTML = `<div class="ask-pick-cover" style="background-image:url('${escapeAttr(a.cover_image_url)}')"></div>`;
        }
        const ctx = a.pitch_line || a.subtitle || '';
        html += `<div class="ask-pick-card" onclick="openArticle(${a.id})">
          ${coverHTML}
          <div class="ask-pick-body">
            <div class="ask-pick-title">${escapeHTML(a.title)}</div>
            <div class="ask-pick-author">${escapeHTML(a.author || '')}</div>
            ${ctx ? `<div class="ask-pick-ctx">${escapeHTML(ctx)}</div>` : ''}
          </div>
        </div>`;
      }
      html += '</div>';
    }

    // Show saved-as-issue badge.
    const issueId = data.issue ? data.issue.id : (data.cached ? data.issue?.id : null);
    const issueTitle = data.issue ? data.issue.title : (data.title || '');
    if (issueId || data.cached) {
      const cachedLabel = data.cached ? ' · from archive' : '';
      html += `<div class="ask-saved">Saved as "<a href="#" onclick="event.preventDefault();openIssue(${issueId || data.issue?.id})">${escapeHTML(issueTitle)}</a>"${cachedLabel}</div>`;
    }

    // New question button so the user can ask again without scrolling back up.
    html += `<div class="ask-again"><button class="ask-again-btn" onclick="clearAskResult()">Ask another question</button></div>`;

    resultBox.innerHTML = html;
    $('ask-input').value = '';
  } catch (err) {
    resultBox.innerHTML = `<p class="hint">Something went wrong.</p>`;
  }
});

function clearAskResult() {
  hide($('ask-result'));
  $('ask-input').value = '';
  $('ask-input').focus();
  document.querySelector('.ask-section').scrollIntoView({ behavior: 'smooth' });
}

function escapeRegex(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

// ---------- scroll events (v2 fuel) ----------
window.addEventListener('scroll', throttle(() => {
  if (!current || $('reader').classList.contains('hidden')) return;
  const h = document.documentElement;
  const pct = Math.min(100, Math.round(((h.scrollTop + window.innerHeight) / h.scrollHeight) * 100));
  if (pct > maxScroll) maxScroll = pct;
  if (!readStarted && pct > 15) { readStarted = true; sendEvent('read_started', pct); }
  if (!completedFired && pct >= 90) { completedFired = true; sendEvent('completed', pct); }
}, 800));

function sendEvent(type, pct) {
  if (!current) return;
  api('/api/events', { method: 'POST', body: JSON.stringify({
    article_id: current.id, type, scroll_pct: pct || 0
  })}).catch(() => {});
}

window.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'hidden' && current && readStarted && !completedFired) {
    navigator.sendBeacon('/api/events', JSON.stringify({
      article_id: current.id, type: 'abandoned', scroll_pct: maxScroll
    }));
  }
});

// ---------- issues ----------
async function loadIssues() {
  try {
    const res = await api('/api/issues');
    const issues = await res.json();
    const section = $('issues-section');
    const list = $('issues-list');
    if (!issues || !issues.length) { hide(section); return; }
    show(section);
    list.innerHTML = '';
    for (const issue of issues.slice(0, 6)) {
      const card = document.createElement('div');
      card.className = 'issue-card';
      card.onclick = () => openIssue(issue.id);
      const date = new Date(issue.created_at).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
      card.innerHTML = `<div class="issue-card-title">${escapeHTML(issue.title)}</div>
        <div class="issue-card-meta">${escapeHTML(date)} · ${escapeHTML(issue.query)}</div>`;
      list.appendChild(card);
    }
    if (issues.length > 6) {
      const more = document.createElement('button');
      more.className = 'issue-more';
      more.textContent = `View all ${issues.length} issues`;
      more.onclick = () => openAllIssues();
      list.appendChild(more);
    }
  } catch (e) {}
}

async function openIssue(id) {
  hideAll(); show($('issue-view'));
  window.scrollTo(0, 0);
  const content = $('issue-content');
  content.innerHTML = '<div class="loading">Loading</div>';

  const res = await api('/api/issue/' + id);
  const issue = await res.json();
  const arts = issue.articles || {};

  let html = '';

  // Hero cover from main pick.
  const mainArt = arts[String(issue.main_pick)];
  if (mainArt && mainArt.cover_image_url) {
    html += `<div class="ask-hero" style="background-image:url('${escapeAttr(mainArt.cover_image_url)}')" onclick="openArticle(${mainArt.id})"></div>`;
  } else if (mainArt) {
    html += `<div class="ask-hero ask-hero-gen" style="background-image:${generatedCover(mainArt.title || '')}" onclick="openArticle(${mainArt.id})"></div>`;
  }

  // Title and date.
  const date = new Date(issue.created_at).toLocaleDateString(undefined, { year: 'numeric', month: 'long', day: 'numeric' });
  html += `<div class="issue-header">
    <h2 class="issue-title">${escapeHTML(issue.title)}</h2>
    <div class="issue-date">${escapeHTML(date)} · "${escapeHTML(issue.query)}"</div>
  </div>`;

  // Writeup with inline article links.
  if (issue.writeup) {
    let writeupHTML = escapeHTML(issue.writeup);
    for (const [aid, a] of Object.entries(arts)) {
      const titleRe = new RegExp(escapeRegex(a.title), 'gi');
      const link = `<a class="ask-article-link" href="#" onclick="event.preventDefault();openArticle(${a.id})">${escapeHTML(a.title)}</a>`;
      writeupHTML = writeupHTML.replace(titleRe, link);
    }
    writeupHTML = writeupHTML.split(/\n\n+/).map(p => `<p>${p}</p>`).join('');
    html += `<div class="ask-writeup">${writeupHTML}</div>`;
  }

  // Article pick cards.
  const picks = [issue.main_pick, ...(issue.supporting || [])].filter(Boolean);
  if (picks.length) {
    html += '<div class="ask-picks">';
    for (const pid of picks) {
      const a = arts[String(pid)];
      if (!a) continue;
      let coverHTML = '';
      if (a.cover_image_url) {
        coverHTML = `<div class="ask-pick-cover" style="background-image:url('${escapeAttr(a.cover_image_url)}')"></div>`;
      }
      const ctx = a.pitch_line || a.subtitle || '';
      html += `<div class="ask-pick-card" onclick="openArticle(${a.id})">
        ${coverHTML}
        <div class="ask-pick-body">
          <div class="ask-pick-title">${escapeHTML(a.title)}</div>
          <div class="ask-pick-author">${escapeHTML(a.author || '')}</div>
          ${ctx ? `<div class="ask-pick-ctx">${escapeHTML(ctx)}</div>` : ''}
        </div>
      </div>`;
    }
    html += '</div>';
  }

  // Delete button.
  html += `<div class="issue-actions">
    <button class="issue-delete" onclick="deleteIssue(${issue.id})">Delete this issue</button>
  </div>`;

  content.innerHTML = html;
}

async function deleteIssue(id) {
  await api('/api/issue/' + id, { method: 'DELETE' });
  backToHome();
}

async function openAllIssues() {
  hideAll(); show($('issue-view'));
  window.scrollTo(0, 0);
  const content = $('issue-content');
  content.innerHTML = '<div class="loading">Loading</div>';

  const res = await api('/api/issues');
  const issues = await res.json();

  let html = '<div class="issue-header"><h2 class="issue-title">All Issues</h2></div>';
  if (!issues || !issues.length) {
    html += '<p class="hint" style="text-align:center;padding:40px">No issues yet. Ask a question to create one.</p>';
  } else {
    html += '<div class="issues-archive">';
    for (const issue of issues) {
      const date = new Date(issue.created_at).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
      html += `<div class="issue-archive-card" onclick="openIssue(${issue.id})">
        <div class="issue-card-title">${escapeHTML(issue.title)}</div>
        <div class="issue-card-meta">${escapeHTML(date)} · "${escapeHTML(issue.query)}"</div>
        <div class="issue-card-preview">${escapeHTML((issue.writeup || '').slice(0, 120))}…</div>
      </div>`;
    }
    html += '</div>';
  }
  content.innerHTML = html;
}

// ---------- magazine view ----------
async function openMagazine(threadSlug) {
  hideAll(); show($('magazine'));
  window.scrollTo(0, 0);

  const grid = $('magazine-grid');
  grid.innerHTML = '<div class="loading">Loading</div>';

  const filterBar = $('magazine-filters');
  filterBar.innerHTML = '';

  const magUrl = threadSlug ? '/api/magazine?thread=' + encodeURIComponent(threadSlug) : '/api/magazine';
  const [res, threadsRes] = await Promise.all([api(magUrl), api('/api/threads')]);
  const items = await res.json();
  const threads = await threadsRes.json();

  if (threads && threads.length) {
    const allChip = document.createElement('button');
    allChip.className = 'mag-filter' + (!threadSlug ? ' active' : '');
    allChip.textContent = 'All';
    allChip.onclick = () => openMagazine();
    filterBar.appendChild(allChip);
    for (const t of threads) {
      const chip = document.createElement('button');
      chip.className = 'mag-filter' + (threadSlug === t.slug ? ' active' : '');
      chip.textContent = t.title;
      chip.onclick = () => openMagazine(t.slug);
      filterBar.appendChild(chip);
    }
  }

  grid.innerHTML = '';
  if (!items.length) {
    grid.innerHTML = '<p class="hint" style="grid-column:1/-1;text-align:center;padding:40px">No articles yet. Sync some first.</p>';
    return;
  }

  for (const it of items) {
    const a = it.article;
    const tile = document.createElement('div');
    const isHero = it.tile_size === 'hero';
    tile.className = 'mag-tile mag-' + it.tile_size + (it.completed ? ' mag-completed' : '');
    tile.onclick = () => openArticle(a.id);

    let coverHTML = '';
    if (a.cover_image_url) {
      coverHTML = `<div class="mag-tile-cover" style="background-image:url('${escapeAttr(a.cover_image_url)}')"></div>`;
    } else {
      coverHTML = `<div class="mag-tile-cover mag-tile-cover-gen" style="background-image:${generatedCover(a.title || 'untitled')}"></div>`;
    }

    const parts = [];
    if (a.author) parts.push(a.author);
    if (it.read_time) parts.push(it.read_time + ' min');
    else if (a.word_count) parts.push(readingTime(a.word_count));

    const threadTag = it.thread ? `<span class="mag-tile-thread">${escapeHTML(it.thread)}</span>` : '';
    const completedBadge = it.completed ? '<span class="mag-tile-done">Read</span>' : '';

    if (isHero) {
      // Hero tiles: big cover, prominent title, context, and reading preview.
      const ctxHTML = it.context ? `<p class="mag-hero-ctx">${escapeHTML(it.context)}</p>` : '';
      tile.innerHTML = `${coverHTML}
        <div class="mag-tile-body">
          ${threadTag}
          <h2 class="mag-hero-title">${escapeHTML(a.title)}</h2>
          ${ctxHTML}
          <div class="mag-tile-meta">${escapeHTML(parts.join(' · '))}${completedBadge}</div>
        </div>`;
    } else if (it.tile_size === 'large') {
      const ctxHTML = it.context
        ? `<p class="mag-tile-ctx mag-tile-ctx-lg">${escapeHTML(it.context)}</p>` : '';
      tile.innerHTML = `${coverHTML}
        <div class="mag-tile-body">
          ${threadTag}
          <h3 class="mag-tile-title">${escapeHTML(a.title)}</h3>
          ${ctxHTML}
          <div class="mag-tile-meta">${escapeHTML(parts.join(' · '))}${completedBadge}</div>
        </div>`;
    } else {
      const ctxHTML = it.context && it.tile_size === 'medium'
        ? `<p class="mag-tile-ctx">${escapeHTML(it.context)}</p>` : '';
      tile.innerHTML = `${coverHTML}
        <div class="mag-tile-body">
          ${threadTag}
          <h3 class="mag-tile-title">${escapeHTML(a.title)}</h3>
          ${ctxHTML}
          <div class="mag-tile-meta">${escapeHTML(parts.join(' · '))}${completedBadge}</div>
        </div>`;
    }
    grid.appendChild(tile);
  }
}

// ---------- settings ----------
function openSettings() {
  hideAll(); show($('settings'));
  loadSettings();
}
function closeSettings() { loadHome(); }
$('gear').addEventListener('click', openSettings);

async function loadSettings() {
  const res = await api('/api/settings');
  const s = await res.json();
  $('notify_time').value = s.notify_time || '';
  $('timezone').value = s.timezone || Intl.DateTimeFormat().resolvedOptions().timeZone || '';
  $('saved_list_url').value = s.saved_list_url || '';
  $('anthropic_key').value = anthropicKey();
  $('key-status').textContent = anthropicKey() ? 'Key set in this browser.' : '';
  $('sync-status').textContent = s.hasCookie ? 'Cookie saved. Sync anytime.' : '';
}

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
  $('sync-status').textContent = `Added ${data.new}, skipped ${data.skipped}, failed ${data.failed}.`;
}

// ---------- boring library ----------
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
    d.onclick = () => openArticle(it.id);
    box.appendChild(d);
  }
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
function readingTime(words) {
  const m = Math.max(1, Math.round(words / 220));
  return m + ' min read';
}

function stripToPreview(html, maxChars) {
  if (!html) return '';
  const tmp = document.createElement('div');
  tmp.innerHTML = html;
  const text = tmp.textContent || '';
  if (text.length <= maxChars) return text;
  return text.slice(0, maxChars).replace(/\s+\S*$/, '') + '…';
}

function throttle(fn, ms) {
  let last = 0, t;
  return (...a) => {
    const now = Date.now();
    if (now - last >= ms) { last = now; fn(...a); }
    else { clearTimeout(t); t = setTimeout(() => { last = Date.now(); fn(...a); }, ms - (now - last)); }
  };
}

function escapeHTML(s) {
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

function escapeAttr(s) {
  return s.replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}

// Generate a unique visual from a string (title). Produces a multi-stop
// gradient with colors derived from the text, so every article without a
// cover image gets its own distinct look.
function generatedCover(text) {
  let h = 0;
  for (let i = 0; i < text.length; i++) h = ((h << 5) - h + text.charCodeAt(i)) | 0;
  const abs = Math.abs(h);
  const hue1 = abs % 360;
  const hue2 = (hue1 + 40 + (abs % 60)) % 360;
  const hue3 = (hue2 + 60 + (abs % 80)) % 360;
  const sat = 25 + (abs % 30);
  const light = 12 + (abs % 10);
  const angle = abs % 360;
  return `linear-gradient(${angle}deg, hsl(${hue1},${sat}%,${light}%) 0%, hsl(${hue2},${sat + 10}%,${light + 5}%) 50%, hsl(${hue3},${sat}%,${light + 3}%) 100%)`;
}

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
