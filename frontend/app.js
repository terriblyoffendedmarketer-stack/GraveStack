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
  // Resume reading if an article was open when the app was last closed.
  const lastArticle = localStorage.getItem('gs_reading');
  if (lastArticle) {
    openArticle(parseInt(lastArticle, 10));
  } else {
    loadHome();
  }
}

function showLogin() {
  hideAll(); show($('login'));
}

function hideAll() {
  for (const id of ['login', 'home', 'reader', 'thread-view', 'magazine', 'issue-view', 'taste', 'history', 'settings']) hide($(id));
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

  const today = new Date().toISOString().slice(0, 10);
  const cacheKey = 'gs_home_' + today;

  // Use today's cached picks so the home page stays stable within a day.
  let data;
  const cached = localStorage.getItem(cacheKey);
  if (cached) {
    data = JSON.parse(cached);
  } else {
    // Clear stale day caches.
    for (let i = localStorage.length - 1; i >= 0; i--) {
      const k = localStorage.key(i);
      if (k && k.startsWith('gs_home_') && k !== cacheKey) localStorage.removeItem(k);
    }
    const res = await api('/api/home');
    data = await res.json();
    if (!data.empty) localStorage.setItem(cacheKey, JSON.stringify(data));
  }
  if (data.empty) { show($('empty')); return; }

  renderFeatured(data.featured);
  renderSuggestions(data.suggestions || []);
  renderWriteup(data.writeup);
  renderThreadsNav(data.threads || []);
  loadIssues();

  // Async enrich: generate pitches + writeup in background without blocking render.
  if ((data.pending_pitches && data.pending_pitches.length) || data.pending_writeup) {
    enrichHome(data);
  }
}

async function enrichHome(data) {
  const featuredId = data.featured && data.featured.article ? data.featured.article.id : 0;
  const suggestionIds = (data.suggestions || []).map(s => s.article.id);
  const body = {
    article_ids: data.pending_pitches || [],
    writeup: data.pending_writeup || false,
    featured: featuredId,
    suggestions: suggestionIds,
  };
  try {
    const res = await api('/api/home/enrich', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const enriched = await res.json();

    // Update pitches in the rendered cards.
    if (enriched.pitches) {
      if (enriched.pitches[featuredId]) {
        const pitch = enriched.pitches[featuredId].pitch_line;
        if (pitch) $('featured-context').textContent = pitch;
      }
      const cards = document.querySelectorAll('.suggestion-card');
      (data.suggestions || []).forEach((s, i) => {
        const p = enriched.pitches[s.article.id];
        if (p && p.pitch_line && cards[i]) {
          const ctx = cards[i].querySelector('.suggestion-card-reason');
          if (ctx) ctx.textContent = p.pitch_line;
        }
      });
    }

    if (enriched.writeup) {
      renderWriteup(enriched.writeup);
    }
  } catch (e) {
    // Enrichment is best-effort — page already rendered with fallbacks.
  }
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
  hide($('post-read-feedback'));
  window.scrollTo(0, 0);

  localStorage.setItem('gs_reading', id);
  const res = await api('/api/article/' + id);
  const data = await res.json();
  renderArticle(data.article);
  loadRelated(id);
  loadHighlights(id);
  loadNotes(id);
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

async function shareArticle() {
  if (!current || !current.url) return;
  if (navigator.share) {
    try { await navigator.share({ title: current.title, url: current.url }); } catch (e) {}
  } else {
    await navigator.clipboard.writeText(current.url);
    const btn = document.querySelector('.share-btn');
    const orig = btn.textContent;
    btn.textContent = 'Copied!';
    setTimeout(() => { btn.textContent = orig; }, 1500);
  }
}

function backToHome() {
  localStorage.removeItem('gs_reading');
  loadHome();
}

// ---------- highlights ----------
async function loadHighlights(articleId) {
  const section = $('highlights-section');
  hide(section);
  try {
    const res = await api('/api/article/' + articleId + '/highlights');
    const highlights = await res.json();
    if (!highlights.length) return;
    show(section);
    renderHighlights(highlights, articleId);
  } catch (e) {}
}

function renderHighlights(highlights, articleId) {
  const list = $('highlights-list');
  list.innerHTML = '';
  for (const h of highlights) {
    const el = document.createElement('div');
    el.className = 'highlight-item';
    el.innerHTML = `<blockquote class="highlight-text">${escapeHTML(h.text)}</blockquote>
      ${h.note ? `<p class="highlight-note">${escapeHTML(h.note)}</p>` : ''}
      <button class="highlight-delete" onclick="deleteHighlight(${h.id}, ${articleId})">Remove</button>`;
    list.appendChild(el);
  }
}

async function saveHighlight() {
  const sel = window.getSelection();
  const text = sel.toString().trim();
  if (!text || !current) return;
  hide($('highlight-tooltip'));
  sel.removeAllRanges();
  try {
    await api('/api/article/' + current.id + '/highlights', {
      method: 'POST',
      body: JSON.stringify({ text, note: '' }),
    });
    loadHighlights(current.id);
  } catch (e) {}
}

async function deleteHighlight(id, articleId) {
  try {
    await api('/api/highlight/' + id, { method: 'DELETE' });
    loadHighlights(articleId);
  } catch (e) {}
}

// Show tooltip on text selection within the article body.
document.addEventListener('mouseup', positionHighlightTooltip);
document.addEventListener('touchend', () => setTimeout(positionHighlightTooltip, 100));

function positionHighlightTooltip() {
  const tooltip = $('highlight-tooltip');
  const sel = window.getSelection();
  if (!sel.rangeCount || sel.isCollapsed || !current || $('reader').classList.contains('hidden')) {
    hide(tooltip);
    return;
  }
  const bodyEl = $('body');
  if (!bodyEl.contains(sel.anchorNode)) { hide(tooltip); return; }
  const rect = sel.getRangeAt(0).getBoundingClientRect();
  tooltip.style.top = (rect.top + window.scrollY - 40) + 'px';
  tooltip.style.left = Math.min(rect.left + rect.width / 2 - 60, window.innerWidth - 140) + 'px';
  show(tooltip);
}

document.addEventListener('mousedown', (e) => {
  const tooltip = $('highlight-tooltip');
  if (!tooltip.contains(e.target)) hide(tooltip);
});

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
  if (!completedFired && pct >= 90) {
    completedFired = true;
    sendEvent('completed', pct);
    localStorage.removeItem('gs_reading');
    show($('post-read-feedback'));
  }
}, 800));

function sendEvent(type, pct) {
  if (!current) return;
  api('/api/events', { method: 'POST', body: JSON.stringify({
    article_id: current.id, type, scroll_pct: pct || 0
  })}).catch(() => {});
}

function sendFeedback(type) {
  sendEvent(type, maxScroll);
  const btns = document.querySelector('.feedback-buttons');
  if (btns) btns.innerHTML = '<p class="feedback-thanks">Got it — noted!</p>';
}

async function saveArticleNote() {
  if (!current) return;
  const textarea = $('article-note');
  const text = textarea.value.trim();
  if (!text) return;
  try {
    await api('/api/article/' + current.id + '/notes', {
      method: 'POST',
      body: JSON.stringify({ text }),
    });
    textarea.value = '';
    loadNotes(current.id);
  } catch (e) {}
}

async function loadNotes(articleId) {
  try {
    const res = await api('/api/article/' + articleId + '/notes');
    const notes = await res.json();
    renderNotes(notes);
  } catch (e) {}
}

function renderNotes(notes) {
  let section = document.getElementById('notes-section');
  if (!section) {
    section = document.createElement('section');
    section.id = 'notes-section';
    section.className = 'notes-section';
    const feedback = $('post-read-feedback');
    feedback.parentNode.insertBefore(section, feedback.nextSibling);
  }
  if (!notes.length) { section.innerHTML = ''; return; }
  let html = '<h3 class="section-label">Your notes</h3>';
  for (const n of notes) {
    html += `<div class="note-item"><p class="note-text">${escapeHTML(n.text)}</p></div>`;
  }
  section.innerHTML = html;
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
// ---------- taste profile ----------
async function openTaste() {
  hideAll(); show($('taste'));
  window.scrollTo(0, 0);

  const content = $('taste-content');
  content.innerHTML = '<div class="loading">Loading your profile...</div>';

  try {
    const res = await api('/api/taste');
    const d = await res.json();
    renderTaste(content, d);
  } catch (e) {
    content.innerHTML = '<p class="empty-msg">Could not load taste profile.</p>';
  }
}

function renderTaste(el, d) {
  const n = d.numbers;
  let html = '';

  // Numbers grid
  html += '<div class="taste-numbers">';
  html += tasteNum(n.total_articles, 'Articles saved');
  html += tasteNum(n.completed, 'Completed');
  html += tasteNum(n.loved, 'Loved');
  html += tasteNum(n.highlights, 'Highlights');
  html += tasteNum(n.notes, 'Notes');
  html += tasteNum(n.queries_made, 'Queries');
  html += tasteNum(n.avg_scroll_depth + '%', 'Avg depth');
  html += tasteNum(n.this_month, 'This month');
  html += '</div>';

  // Read length distribution
  const rl = d.read_lengths;
  const rlTotal = rl.short + rl.medium + rl.long || 1;
  html += '<div class="taste-section">';
  html += '<h3 class="taste-section-title">Reading style</h3>';
  html += '<div class="taste-bars">';
  html += tasteBar('Short (&lt;5 min)', rl.short, rlTotal);
  html += tasteBar('Medium (5–15 min)', rl.medium, rlTotal);
  html += tasteBar('Long (15+ min)', rl.long, rlTotal);
  html += '</div></div>';

  // Theme heatmap
  if (d.themes && d.themes.length) {
    html += '<div class="taste-section">';
    html += '<h3 class="taste-section-title">Your themes</h3>';
    html += '<div class="taste-themes">';
    const maxC = d.themes[0].count;
    for (const t of d.themes) {
      const opacity = 0.3 + (t.count / maxC) * 0.7;
      html += `<span class="taste-theme" style="opacity:${opacity}">${escapeHTML(t.theme)} <small>${t.count}</small></span>`;
    }
    html += '</div></div>';
  }

  // Top threads
  if (d.top_threads.length) {
    html += '<div class="taste-section">';
    html += '<h3 class="taste-section-title">Top threads</h3>';
    for (const t of d.top_threads) {
      const pct = Math.round(t.read_count / (t.article_count || 1) * 100);
      html += `<div class="taste-row">
        <span class="taste-row-icon">${t.icon || ''}</span>
        <span class="taste-row-label">${escapeHTML(t.title)}</span>
        <span class="taste-row-stat">${t.read_count} read · ${pct}%</span>
      </div>`;
    }
    html += '</div>';
  }

  // Top authors
  if (d.top_authors.length) {
    html += '<div class="taste-section">';
    html += '<h3 class="taste-section-title">Favorite authors</h3>';
    for (const a of d.top_authors) {
      html += `<div class="taste-row">
        <span class="taste-row-label">${escapeHTML(a.name)}</span>
        <span class="taste-row-stat">${a.read_count} read of ${a.articles}</span>
      </div>`;
    }
    html += '</div>';
  }

  // Most-engaged articles
  if (d.top_articles.length) {
    html += '<div class="taste-section">';
    html += '<h3 class="taste-section-title">Most engaged</h3>';
    for (const a of d.top_articles) {
      html += `<div class="taste-row taste-row-clickable" onclick="openArticle(${a.id})">
        <span class="taste-row-label">${escapeHTML(a.title)}</span>
        <span class="taste-row-stat">${escapeHTML(a.author)}</span>
      </div>`;
    }
    html += '</div>';
  }

  // Queries
  if (d.queries.length) {
    html += '<div class="taste-section">';
    html += '<h3 class="taste-section-title">Your queries</h3>';
    for (const q of d.queries) {
      html += `<div class="taste-row">
        <span class="taste-row-label">${escapeHTML(q.title)}</span>
      </div>`;
    }
    html += '</div>';
  }

  el.innerHTML = html;
}

function tasteNum(val, label) {
  return `<div class="taste-num"><div class="taste-num-val">${val}</div><div class="taste-num-label">${label}</div></div>`;
}

function tasteBar(label, count, total) {
  const pct = Math.round(count / total * 100);
  return `<div class="taste-bar-row">
    <span class="taste-bar-label">${label}</span>
    <div class="taste-bar-track"><div class="taste-bar-fill" style="width:${pct}%"></div></div>
    <span class="taste-bar-val">${count}</span>
  </div>`;
}

// ---------- reading history ----------
async function openHistory() {
  hideAll(); show($('history'));
  window.scrollTo(0, 0);

  const content = $('history-content');
  content.innerHTML = '<div class="loading">Loading...</div>';

  try {
    const res = await api('/api/history');
    const items = await res.json();
    if (!items.length) {
      content.innerHTML = '<p class="empty-msg">No reading history yet.</p>';
      return;
    }
    let html = '';
    for (const h of items) {
      const date = h.last_read ? new Date(h.last_read).toLocaleDateString() : '';
      let coverHTML = '';
      if (h.cover_image_url) {
        coverHTML = `<div class="history-cover" style="background-image:url('${escapeAttr(h.cover_image_url)}')"></div>`;
      }
      html += `<div class="history-item" onclick="openArticle(${h.id})">
        ${coverHTML}
        <div class="history-info">
          <div class="history-title">${escapeHTML(h.title)}</div>
          <div class="history-meta">${escapeHTML(h.author || '')}${date ? ' · ' + date : ''}</div>
        </div>
      </div>`;
    }
    content.innerHTML = html;
  } catch (e) {
    content.innerHTML = '<p class="empty-msg">Could not load history.</p>';
  }
}

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
  const grad = `linear-gradient(${angle}deg, hsl(${hue1},${sat}%,${light}%) 0%, hsl(${hue2},${sat + 10}%,${light + 5}%) 50%, hsl(${hue3},${sat}%,${light + 3}%) 100%)`;

  // Add geometric shapes via SVG for visual interest.
  const shapes = [];
  const shapeCount = 2 + (abs % 3);
  for (let i = 0; i < shapeCount; i++) {
    const seed = (abs * (i + 7)) >>> 0;
    const x = 10 + (seed % 80);
    const y = 10 + ((seed >> 8) % 80);
    const size = 20 + (seed % 40);
    const opacity = 0.06 + (seed % 8) / 100;
    const shapeType = seed % 3;
    if (shapeType === 0) {
      shapes.push(`<circle cx="${x}%" cy="${y}%" r="${size}" fill="white" opacity="${opacity}"/>`);
    } else if (shapeType === 1) {
      shapes.push(`<rect x="${x - 5}%" y="${y - 5}%" width="${size}" height="${size}" fill="white" opacity="${opacity}" transform="rotate(${seed % 45} ${x} ${y})"/>`);
    } else {
      const pts = `${x},${y - size / 2} ${x - size / 2},${y + size / 2} ${x + size / 2},${y + size / 2}`;
      shapes.push(`<polygon points="${pts}" fill="white" opacity="${opacity}"/>`);
    }
  }

  // First letter as a large watermark.
  const letter = (text[0] || '').toUpperCase();
  const letterX = 50 + (abs % 30) - 15;
  const letterY = 50 + ((abs >> 4) % 30) - 15;
  shapes.push(`<text x="${letterX}%" y="${letterY}%" font-size="120" font-family="serif" fill="white" opacity="0.05" text-anchor="middle" dominant-baseline="central">${letter}</text>`);

  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="100%" height="100%">${shapes.join('')}</svg>`;
  const encoded = 'url("data:image/svg+xml,' + encodeURIComponent(svg) + '")';
  return `${encoded}, ${grad}`;
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
