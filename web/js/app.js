// Shared helpers used by every page.

const TOKEN_KEY = 'vidwatch_token';          // Cognito access token
const REFRESH_KEY = 'vidwatch_refresh';
const EXP_KEY = 'vidwatch_token_exp';        // access token expiry, ms
const USER_KEY = 'vidwatch_user';
const OAUTH_KEY = 'vidwatch_oauth';          // {state, verifier, next} for the login round trip (sessionStorage)
const CONFIG_KEY = 'vidwatch_config';        // sessionStorage

const RATINGS = ['G', 'PG', 'PG-13', 'R'];
const RATING_RANK = { 'G': 0, 'PG': 1, 'PG-13': 2, 'R': 3 };

// ---------- Local preferences (this browser) ----------
// history & playback speed also sync to the account when signed in.
const Settings = {
  get theme() { return localStorage.getItem('vidwatch_theme') || 'system'; },        // 'system' | 'light' | 'dark'
  set theme(v) { localStorage.setItem('vidwatch_theme', v); applyTheme(); },
  get restricted() { return localStorage.getItem('vidwatch_restricted') === '1'; },   // "kids mode"
  set restricted(v) { localStorage.setItem('vidwatch_restricted', v ? '1' : '0'); },
  get maxRating() { const r = localStorage.getItem('vidwatch_maxRating'); return RATINGS.includes(r) ? r : 'PG'; },
  set maxRating(v) { localStorage.setItem('vidwatch_maxRating', v); },
  get recsOff() { return localStorage.getItem('vidwatch_recsOff') === '1'; },
  set recsOff(v) { localStorage.setItem('vidwatch_recsOff', v ? '1' : '0'); },
  // recommendations are hidden in kids mode regardless of the recsOff toggle
  get hideRecs() { return this.restricted || this.recsOff; },
  get volume() { const v = parseFloat(localStorage.getItem('vidwatch_volume')); return Number.isFinite(v) ? v : 1; },
  set volume(v) { localStorage.setItem('vidwatch_volume', String(v)); },
  get muted() { return localStorage.getItem('vidwatch_muted') === '1'; },
  set muted(v) { localStorage.setItem('vidwatch_muted', v ? '1' : '0'); },
  get localSpeed() { const v = parseFloat(localStorage.getItem('vidwatch_localSpeed')); return Number.isFinite(v) ? v : 1; },
  set localSpeed(v) { localStorage.setItem('vidwatch_localSpeed', String(v)); },
  // resume position for signed-out viewers (signed-in viewers use the server)
  getLocalPosition(id) { const v = parseFloat(localStorage.getItem('vidwatch_pos_' + id)); return Number.isFinite(v) ? v : 0; },
  setLocalPosition(id, t) { try { localStorage.setItem('vidwatch_pos_' + id, String(t)); } catch {} },
  clearLocalPosition(id) { localStorage.removeItem('vidwatch_pos_' + id); }
};

function resolvedTheme() {
  const t = Settings.theme;
  if (t === 'light' || t === 'dark') return t;
  return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}
function applyTheme() { document.documentElement.dataset.theme = resolvedTheme(); }
applyTheme();
if (window.matchMedia) {
  try {
    window.matchMedia('(prefers-color-scheme: dark)')
      .addEventListener('change', () => { if (Settings.theme === 'system') applyTheme(); });
  } catch { /* older browsers */ }
}

// ---------- Recent searches (this browser) ----------
const RecentSearches = {
  _KEY: 'vidwatch_recent_searches',
  _MAX: 8,
  get list() {
    try { const a = JSON.parse(localStorage.getItem(this._KEY) || '[]'); return Array.isArray(a) ? a.filter(s => typeof s === 'string' && s.trim()) : []; }
    catch { return []; }
  },
  add(q) {
    q = String(q || '').trim();
    if (!q) return;
    const next = [q, ...this.list.filter(s => s.toLowerCase() !== q.toLowerCase())].slice(0, this._MAX);
    try { localStorage.setItem(this._KEY, JSON.stringify(next)); } catch { /* quota — ignore */ }
  },
  remove(q) {
    const next = this.list.filter(s => s.toLowerCase() !== String(q).toLowerCase());
    try { localStorage.setItem(this._KEY, JSON.stringify(next)); } catch { /* ignore */ }
  },
  clear() { try { localStorage.removeItem(this._KEY); } catch { /* ignore */ } }
};

// ---------- Site config ----------
// { cognitoDomain, clientId, maxUploadMB } from the API, cached for the tab.
let configPromise = null;
function getConfig() {
  if (!configPromise) {
    configPromise = (async () => {
      try {
        const cached = JSON.parse(sessionStorage.getItem(CONFIG_KEY) || 'null');
        if (cached && cached.cognitoDomain && cached.clientId) return cached;
      } catch { /* fetch it */ }
      const res = await fetch('/api/config');
      if (!res.ok) throw new Error('Couldn’t load site settings. Try again in a moment.');
      const cfg = await res.json();
      // accept the domain with or without a scheme, never with a trailing slash
      cfg.cognitoDomain = String(cfg.cognitoDomain || '').replace(/\/+$/, '');
      if (cfg.cognitoDomain && !/^https?:\/\//.test(cfg.cognitoDomain)) cfg.cognitoDomain = 'https://' + cfg.cognitoDomain;
      try { sessionStorage.setItem(CONFIG_KEY, JSON.stringify(cfg)); } catch { /* ignore */ }
      return cfg;
    })();
    configPromise.catch(() => { configPromise = null; });   // let the next caller retry
  }
  return configPromise;
}

// ---------- Auth ----------
// Cognito managed login: authorization code + PKCE. The access token goes to
// our API; the refresh token keeps it fresh for up to 30 days.
function base64url(bytes) {
  let s = '';
  new Uint8Array(bytes).forEach(b => { s += String.fromCharCode(b); });
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}
function randomString(nBytes) { return base64url(crypto.getRandomValues(new Uint8Array(nBytes))); }

// Only same-site paths are allowed as a post-login destination.
function safeNextPath(next) {
  next = String(next || '');
  if (!next.startsWith('/') || next.startsWith('//') || next.startsWith('/\\')) return '/';
  if (/^\/(login|register|auth\/callback)(?:[/?#]|$)/.test(next)) return '/';
  return next;
}

async function tokenRequest(params) {
  const cfg = await getConfig();
  const res = await fetch(`${cfg.cognitoDomain}/oauth2/token`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({ client_id: cfg.clientId, ...params })
  });
  let data = null;
  try { data = await res.json(); } catch { /* no body */ }
  if (!res.ok) {
    const err = new Error((data && (data.error_description || data.error)) || `Sign-in failed (${res.status})`);
    err.status = res.status;
    throw err;
  }
  return data;
}

const Auth = {
  get token() { return localStorage.getItem(TOKEN_KEY); },
  get refreshToken() { return localStorage.getItem(REFRESH_KEY); },
  get expiresAt() { return Number(localStorage.getItem(EXP_KEY)) || 0; },
  get user() {
    const raw = localStorage.getItem(USER_KEY);
    if (!raw) return null;
    try { return JSON.parse(raw); } catch { return null; }
  },
  setTokens(t) {
    localStorage.setItem(TOKEN_KEY, t.access_token);
    localStorage.setItem(EXP_KEY, String(Date.now() + (Number(t.expires_in) || 3600) * 1000));
    // Cognito only returns a new refresh token if rotation is on; otherwise keep ours
    if (t.refresh_token) localStorage.setItem(REFRESH_KEY, t.refresh_token);
  },
  setUser(user) { localStorage.setItem(USER_KEY, JSON.stringify(user)); },
  clear() {
    localStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(REFRESH_KEY);
    localStorage.removeItem(EXP_KEY);
    localStorage.removeItem(USER_KEY);
  },

  // Send the browser to Cognito's sign-in page (it links to sign-up too).
  async login(next) {
    const cfg = await getConfig();
    const state = randomString(16);
    const verifier = randomString(32);
    const challenge = base64url(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier)));
    sessionStorage.setItem(OAUTH_KEY, JSON.stringify({ state, verifier, next: safeNextPath(next) }));
    const q = new URLSearchParams({
      response_type: 'code',
      client_id: cfg.clientId,
      redirect_uri: location.origin + '/auth/callback',
      scope: 'openid email profile',
      state,
      code_challenge_method: 'S256',
      code_challenge: challenge
    });
    location.href = `${cfg.cognitoDomain}/oauth2/authorize?${q}`;
  },

  // Called by /auth/callback: trade the code for tokens, then load our profile.
  async completeLogin(code, verifier) {
    const t = await tokenRequest({
      grant_type: 'authorization_code',
      code,
      redirect_uri: location.origin + '/auth/callback',
      code_verifier: verifier
    });
    this.setTokens(t);
    try {
      const { user } = await api('/api/auth/me');
      this.setUser(user);
      return user;
    } catch (e) { this.clear(); throw e; }
  },

  // A usable access token, refreshing it first if it's about to expire.
  async freshToken() {
    if (!this.token) return null;
    if (this.expiresAt - Date.now() > 60 * 1000) return this.token;
    if (await refreshTokens()) return this.token;
    return this.expiresAt > Date.now() ? this.token : null;
  },

  async logout() {
    let cfg = null;
    try { cfg = await getConfig(); } catch { /* still sign out locally */ }
    const rt = this.refreshToken;
    if (cfg && rt) {
      try {
        await fetch(`${cfg.cognitoDomain}/oauth2/revoke`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          body: new URLSearchParams({ token: rt, client_id: cfg.clientId })
        });
      } catch { /* best effort */ }
    }
    this.clear();
    // end the Cognito session too, or the next "Sign in" would skip the password
    location.href = cfg
      ? `${cfg.cognitoDomain}/logout?client_id=${encodeURIComponent(cfg.clientId)}&logout_uri=${encodeURIComponent(location.origin + '/')}`
      : '/';
  }
};

// One refresh at a time, shared by every caller that needs it.
let refreshPromise = null;
function refreshTokens() {
  if (!refreshPromise) {
    refreshPromise = (async () => {
      const rt = Auth.refreshToken;
      if (!rt) return false;
      try {
        Auth.setTokens(await tokenRequest({ grant_type: 'refresh_token', refresh_token: rt }));
        return true;
      } catch (e) {
        // a rejected refresh token means the session is over; a network blip doesn't
        if (e.status === 400 || e.status === 401) { Auth.clear(); refreshTopbar(); }
        return false;
      }
    })().finally(() => { refreshPromise = null; });
  }
  return refreshPromise;
}

function requireLogin(nextPath) {
  if (!Auth.user) {
    location.href = '/login?next=' + encodeURIComponent(nextPath || (location.pathname + location.search));
    return false;
  }
  return true;
}

// ---------- Play queue (this tab) ----------
// Video ids are strings.
const Queue = {
  get ids() { try { const a = JSON.parse(sessionStorage.getItem('vidwatch_queue') || '[]'); return Array.isArray(a) ? a.map(String) : []; } catch { return []; } },
  set ids(arr) { sessionStorage.setItem('vidwatch_queue', JSON.stringify(arr.map(String))); },
  get count() { return this.ids.length; },
  has(id) { return this.ids.includes(String(id)); },
  add(id) { id = String(id); const a = this.ids.filter(x => x !== id); a.push(id); this.ids = a; refreshTopbar(); },
  remove(id) { id = String(id); const a = this.ids.filter(x => x !== id); this.ids = a; refreshTopbar(); },
  // drop `id` and everything queued before it — used when you start watching a queued video
  consume(id) { id = String(id); const a = this.ids; const i = a.indexOf(id); if (i >= 0) { this.ids = a.slice(i + 1); refreshTopbar(); } },
  clear() { this.ids = []; refreshTopbar(); }
};

// ---------- API ----------
// JSON in, JSON out. The access token only ever goes to our own /api/ routes.
async function api(path, { method = 'GET', body } = {}, retried = false) {
  const headers = {};
  const ours = String(path).startsWith('/api/');
  let token = null;
  if (ours && Auth.token) {
    token = await Auth.freshToken();
    if (token) headers['Authorization'] = `Bearer ${token}`;
  }
  let payload;
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }
  const res = await fetch(path, { method, headers, body: payload });
  if (res.status === 401 && token && !retried && await refreshTokens()) {
    return api(path, { method, body }, true);
  }
  let data = null;
  try { data = await res.json(); } catch { /* no body */ }
  if (!res.ok) {
    const err = new Error((data && data.error) || `Request failed (${res.status})`);
    err.status = res.status;
    if (res.status === 401 && token) { Auth.clear(); refreshTopbar(); }
    throw err;
  }
  return data;
}

// ---------- Uploads ----------
// Files go straight from the browser to S3 with a presigned POST from our API;
// the API never sees the bytes. Resolves with the object key to pass to the
// next API call. `onProgress(fraction)` is optional, and the returned promise
// has an abort() method.
function uploadFile(file, kind, onProgress) {
  let xhr = null;
  let aborted = false;
  const p = (async () => {
    const { url, fields, key } = await api('/api/uploads', {
      method: 'POST',
      body: { kind, contentType: file.type, size: file.size }
    });
    if (aborted) throw new Error('Upload cancelled.');
    return new Promise((resolve, reject) => {
      const fd = new FormData();
      for (const [k, v] of Object.entries(fields || {})) fd.append(k, v);
      fd.append('file', file, file.name || 'upload');   // S3 wants the file last
      xhr = new XMLHttpRequest();
      xhr.open('POST', url);   // no Authorization header: the signed fields are the credential
      if (onProgress) {
        xhr.upload.addEventListener('progress', (ev) => { if (ev.lengthComputable) onProgress(ev.loaded / ev.total); });
      }
      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) { if (onProgress) onProgress(1); resolve(key); return; }
        // S3 errors are XML: <Error><Code>…</Code><Message>…</Message></Error>
        const m = /<Message>([\s\S]*?)<\/Message>/.exec(xhr.responseText || '');
        reject(new Error(m ? `Upload failed: ${m[1]}` : `Upload failed (${xhr.status}).`));
      };
      xhr.onerror = () => reject(new Error('Network error during upload.'));
      xhr.onabort = () => reject(new Error('Upload cancelled.'));
      xhr.send(fd);
    });
  })();
  p.abort = () => { aborted = true; if (xhr) xhr.abort(); };
  return p;
}

// ---------- Formatting helpers ----------
function timeAgo(ms) {
  const diff = Date.now() - ms;
  const s = Math.max(1, Math.floor(diff / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60); if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60); if (h < 24) return `${h}h ago`;
  const d = Math.floor(h / 24); if (d < 30) return `${d}d ago`;
  const mo = Math.floor(d / 30); if (mo < 12) return `${mo}mo ago`;
  return `${Math.floor(mo / 12)}y ago`;
}

function pluralize(n, word) {
  n = Number(n) || 0;
  return `${n.toLocaleString()} ${word}${n === 1 ? '' : 's'}`;
}

function fmtDuration(seconds) {
  if (seconds == null || !isFinite(seconds) || seconds < 0) return '';
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = s % 60;
  const mm = h ? String(m).padStart(2, '0') : String(m);
  return (h ? h + ':' : '') + mm + ':' + String(sec).padStart(2, '0');
}

function parseTimecode(str) {
  const parts = String(str).split(':').map(Number);
  if (parts.some(p => !Number.isFinite(p))) return null;
  if (parts.length === 3) return parts[0] * 3600 + parts[1] * 60 + parts[2];
  if (parts.length === 2) return parts[0] * 60 + parts[1];
  return null;
}

function escapeHtml(str) {
  return String(str ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

// Turn timestamp tokens like "1:23" or "01:02:03" inside already-escaped text
// into clickable links that seek the player. Safe: operates on escaped text.
function linkifyTimestamps(escapedText) {
  return escapedText.replace(/\b(?:\d{1,2}:)?\d{1,2}:\d{2}\b/g, (m) => {
    const t = parseTimecode(m);
    if (t == null) return m;
    return `<a href="#" class="ts-link" data-t="${t}">${m}</a>`;
  });
}

// Ratings are still stored and used by kids mode; this only controls the visible badge.
const SHOW_RATING_BADGES = false;
function ratingClass(r) { return 'r-' + String(r || 'G').toLowerCase().replace(/-/g, ''); } // 'PG-13' -> 'r-pg13'
function ratingBadge(r) {
  if (!SHOW_RATING_BADGES) return '';
  r = RATINGS.includes(r) ? r : 'G';
  return `<span class="rating-badge ${ratingClass(r)}" title="Rated ${escapeHtml(r)} by the creator">${escapeHtml(r)}</span>`;
}
function isNew(createdAt) { return Date.now() - createdAt < 24 * 60 * 60 * 1000; }
function videoPassesRestriction(v) {
  if (!Settings.restricted) return true;
  return RATING_RANK[v.ageRating] <= RATING_RANK[Settings.maxRating];
}

function avatarFor(userOrName, opts = {}) {
  const cls = `avatar${opts.xl ? ' avatar-xl' : opts.large ? ' avatar-lg' : ''}`;
  const obj = (userOrName && typeof userOrName === 'object') ? userOrName : { username: userOrName };
  if (obj.avatarUrl) {
    return `<span class="${cls} avatar-img" style="background-image:url('${encodeURI(obj.avatarUrl)}')"></span>`;
  }
  const initial = String(obj.username || '?').charAt(0).toUpperCase();
  return `<span class="${cls}">${escapeHtml(initial)}</span>`;
}

// ---------- Video card ----------
function renderVideoCard(v, opts = {}) {
  const { showAuthor = true, progress = null, queueBtn = true, watchLaterBtn = true, extraActions = '' } = opts;
  const pct = (progress && v.durationSeconds) ? Math.min(100, Math.max(0, (progress / v.durationSeconds) * 100)) : 0;
  const tags = [];
  if (v.visibility === 'unlisted') tags.push('<span class="tag">Unlisted</span>');
  if (isNew(v.createdAt)) tags.push('<span class="new-badge">NEW</span>');
  return `
    <div class="card" data-vid="${escapeHtml(v.id)}">
      <a class="card-link" href="/watch?v=${encodeURIComponent(v.id)}">
        <div class="thumb"
             style="${v.thumbnailUrl ? `background-image:url('${encodeURI(v.thumbnailUrl)}')` : ''}">
          ${v.thumbnailUrl ? '' : `<video class="thumb-video" src="${encodeURI(v.videoUrl)}#t=0.1" preload="metadata" muted playsinline></video>`}
          ${ratingBadge(v.ageRating)}
          ${v.durationSeconds ? `<span class="duration-badge">${fmtDuration(v.durationSeconds)}</span>` : ''}
          ${pct ? `<span class="card-progress"><span style="width:${pct}%"></span></span>` : ''}
        </div>
        <div class="card-info">
          ${showAuthor ? avatarFor(v.author) : ''}
          <div class="card-text">
            <div class="card-title">${escapeHtml(v.title)}</div>
            <div class="card-meta">
              ${showAuthor ? `<span class="card-author">${escapeHtml(v.author.username)}</span>` : ''}
              <div>${tags.join(' ')} ${pluralize(v.views, 'view')} &middot; ${timeAgo(v.createdAt)}</div>
            </div>
          </div>
        </div>
      </a>
      <div class="card-actions">
        ${watchLaterBtn && Auth.user ? `<button class="card-btn" type="button" data-watchlater="${escapeHtml(v.id)}" title="Save to Watch later">+ Later</button>` : ''}
        ${queueBtn && Auth.user ? `<button class="card-btn" type="button" data-queue="${escapeHtml(v.id)}" title="Add to queue">+ Queue</button>` : ''}
        ${extraActions}
      </div>
    </div>
  `;
}

// Render a list of videos into `el`, applying the kids-mode rating filter.
function renderGrid(el, videos, opts = {}) {
  const visible = videos.filter(videoPassesRestriction);
  el.innerHTML = visible.map(v => renderVideoCard(v, opts)).join('');
  return visible.length;
}

// ---------- Misc UI ----------
function toast(message, kind = '') {
  let el = document.querySelector('.toast');
  if (!el) {
    el = document.createElement('div');
    el.className = 'toast';
    document.body.appendChild(el);
  }
  el.className = `toast ${kind}`;
  el.textContent = message;
  requestAnimationFrame(() => el.classList.add('show'));
  clearTimeout(toast._t);
  toast._t = setTimeout(() => el.classList.remove('show'), 2600);
}

async function copyToClipboard(text) {
  try { await navigator.clipboard.writeText(text); return true; } catch { /* fall through */ }
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.focus(); ta.select();
    const ok = document.execCommand('copy');
    ta.remove();
    if (ok) return true;
  } catch { /* fall through */ }
  prompt('Copy this link:', text);
  return false;
}

// Lightweight dropdown menu attached to a trigger button.
// items: [{ label, onClick }] — a null entry renders a divider.
function openMenu(triggerEl, items) {
  closeMenu();
  const menu = document.createElement('div');
  menu.className = 'menu';
  menu.innerHTML = items.map((it, i) =>
    it ? `<button class="menu-item" data-i="${i}" type="button">${escapeHtml(it.label)}</button>` : '<div class="menu-divider"></div>'
  ).join('');
  document.body.appendChild(menu);
  const r = triggerEl.getBoundingClientRect();
  menu.style.top = `${window.scrollY + r.bottom + 6}px`;
  // align right edge of menu under right edge of trigger when possible
  const left = Math.max(8, Math.min(window.scrollX + r.right - menu.offsetWidth, window.scrollX + window.innerWidth - menu.offsetWidth - 8));
  menu.style.left = `${left}px`;
  menu.addEventListener('click', (e) => {
    const btn = e.target.closest('.menu-item');
    if (!btn) return;
    const it = items[Number(btn.dataset.i)];
    closeMenu();
    if (it && it.onClick) it.onClick();
  });
  setTimeout(() => document.addEventListener('mousedown', onDocDown), 0);
  function onDocDown(e) { if (!menu.contains(e.target) && e.target !== triggerEl) closeMenu(); }
  openMenu._cleanup = () => { document.removeEventListener('mousedown', onDocDown); menu.remove(); };
}
function closeMenu() { if (openMenu._cleanup) { openMenu._cleanup(); openMenu._cleanup = null; } }
window.addEventListener('resize', closeMenu);

// ---------- Top bar ----------
function refreshTopbar() { try { renderTopbar(); } catch {} }

function renderTopbar() {
  const slot = document.getElementById('topbar');
  if (!slot) return;
  const user = Auth.user;
  const search = new URLSearchParams(location.search).get('q') || '';
  const themeIcon = { light: '☀', dark: '☾', system: '◐' }[Settings.theme] || '◐';
  const kids = Settings.restricted;
  slot.innerHTML = `
    <header class="topbar">
      <a href="/" class="brand">
        <span class="brand-mark"></span>
        <span class="brand-name">vidwatch</span>
      </a>
      ${kids ? '' : `
      <form class="search" id="search-form" role="search">
        <input id="search-input" name="q" type="search" placeholder="Search videos  (press /)" value="${escapeHtml(search)}" autocomplete="off" />
        <div class="search-suggest" id="search-suggest" hidden></div>
      </form>`}
      <nav class="nav-actions">
        <a class="btn btn-ghost btn-icon" href="/trending" title="Trending">Trending</a>
        ${kids ? '<a class="btn btn-ghost kids-pill" href="/settings" title="Kids mode is on">Kids mode</a>' : ''}
        ${Queue.count ? `<a class="btn btn-ghost queue-chip" href="/watch?v=${encodeURIComponent(Queue.ids[0])}" title="Play queue">&#9654; Queue (${Queue.count})</a>` : ''}
        <button class="btn btn-ghost btn-icon" id="theme-btn" type="button" title="Theme: ${escapeHtml(Settings.theme)} (click to change)">${themeIcon}</button>
        <a class="btn btn-ghost btn-icon" href="/settings" title="Settings">&#9881;</a>
        ${user ? `
          <a class="btn" href="/upload">Upload</a>
          <button class="btn btn-ghost btn-icon notif-btn" id="notif-btn" type="button" title="Notifications" aria-label="Notifications">
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 8a6 6 0 0 1 12 0c0 7 3 9 3 9H3s3-2 3-9"/><path d="M10.3 21a1.94 1.94 0 0 0 3.4 0"/></svg>
            <span class="notif-count" id="notif-count"${Notifs.unread ? '' : ' hidden'}>${Notifs.unread > 9 ? '9+' : Notifs.unread}</span>
          </button>
          <a class="btn btn-ghost btn-icon" href="/library" title="Your library">Library</a>
          <a class="user-chip" href="/u/${encodeURIComponent(user.username)}" title="${escapeHtml(user.username)}">
            ${avatarFor(user)}
            <span>${escapeHtml(user.username)}</span>
          </a>
          <button class="btn btn-ghost" id="logout-btn" type="button">Sign out</button>
        ` : `
          <a class="btn btn-ghost" href="/login">Sign in</a>
          <a class="btn btn-primary" href="/register">Sign up</a>
        `}
      </nav>
    </header>
  `;
  const form = document.getElementById('search-form');
  if (form) setupSearch(form);
  const themeBtn = document.getElementById('theme-btn');
  if (themeBtn) themeBtn.addEventListener('click', () => {
    const order = ['system', 'light', 'dark'];
    Settings.theme = order[(order.indexOf(Settings.theme) + 1) % order.length];
    renderTopbar();
    toast(`Theme: ${Settings.theme}`);
  });
  const logoutBtn = document.getElementById('logout-btn');
  if (logoutBtn) logoutBtn.addEventListener('click', () => { logoutBtn.disabled = true; Auth.logout(); });
  const notifBtn = document.getElementById('notif-btn');
  if (notifBtn) notifBtn.addEventListener('click', () => toggleNotifPanel(notifBtn));
}

// ---------- Notifications ----------
// New uploads from creators you've saved. The server works out what's unread;
// opening the panel marks everything as seen.
const Notifs = {
  items: [],
  unread: 0,
  async load() {
    if (!Auth.user) return;
    try {
      const { items } = await api('/api/me/notifications');
      this.items = items.filter(it => videoPassesRestriction(it.video));
      this.unread = this.items.filter(it => it.unread).length;
      this.paintCount();
    } catch { /* leave the last known state */ }
  },
  paintCount() {
    const el = document.getElementById('notif-count');
    if (!el) return;
    el.hidden = !this.unread;
    el.textContent = this.unread > 9 ? '9+' : String(this.unread);
  }
};

function closeNotifPanel() { if (toggleNotifPanel._cleanup) { toggleNotifPanel._cleanup(); toggleNotifPanel._cleanup = null; } }

async function toggleNotifPanel(btn) {
  if (toggleNotifPanel._cleanup) return closeNotifPanel();
  closeMenu();
  const panel = document.createElement('div');
  panel.className = 'notif-panel';
  panel.innerHTML = '<div class="notif-head">Notifications</div><div class="notif-empty">Loading…</div>';
  document.body.appendChild(panel);
  const place = () => {
    const r = btn.getBoundingClientRect();
    panel.style.top = `${window.scrollY + r.bottom + 6}px`;
    panel.style.left = `${Math.max(8, Math.min(window.scrollX + r.right - panel.offsetWidth, window.scrollX + window.innerWidth - panel.offsetWidth - 8))}px`;
  };
  place();
  const onDocDown = (e) => { if (!panel.contains(e.target) && !btn.contains(e.target)) closeNotifPanel(); };
  const onKey = (e) => { if (e.key === 'Escape') closeNotifPanel(); };
  setTimeout(() => document.addEventListener('mousedown', onDocDown), 0);
  document.addEventListener('keydown', onKey);
  window.addEventListener('resize', closeNotifPanel);
  toggleNotifPanel._cleanup = () => {
    document.removeEventListener('mousedown', onDocDown);
    document.removeEventListener('keydown', onKey);
    window.removeEventListener('resize', closeNotifPanel);
    panel.remove();
  };

  await Notifs.load();
  if (!panel.isConnected) return;
  // unread dots stay visible while the panel is open, even though they're now marked seen
  panel.innerHTML = '<div class="notif-head">Notifications</div>' + (Notifs.items.length
    ? `<div class="notif-list">${Notifs.items.map(({ video: v, unread }) => `
        <a class="notif-item${unread ? ' unread' : ''}" href="/watch?v=${encodeURIComponent(v.id)}">
          <span class="notif-thumb" style="${v.thumbnailUrl ? `background-image:url('${encodeURI(v.thumbnailUrl)}')` : ''}"></span>
          <span class="notif-text">
            <span class="notif-line"><strong>${escapeHtml(v.author.username)}</strong> uploaded ${escapeHtml(v.title)}</span>
            <span class="notif-when">${timeAgo(v.createdAt)}</span>
          </span>
        </a>`).join('')}</div>`
    : '<div class="notif-empty">Nothing yet. Save a creator and their new uploads show up here.</div>');
  place();
  if (Notifs.unread) {
    api('/api/me/notifications/seen', { method: 'POST' }).catch(() => {});
    Notifs.unread = 0;
    Notifs.paintCount();
  }
}

// Search box: submit, plus a "recent searches" dropdown.
function setupSearch(form) {
  const input = form.querySelector('#search-input');
  const suggest = form.querySelector('#search-suggest');
  if (!input) return;

  const go = (q) => { q = String(q || '').trim(); RecentSearches.add(q); location.href = q ? `/?q=${encodeURIComponent(q)}` : '/'; };
  const hide = () => { if (suggest) { suggest.hidden = true; suggest.innerHTML = ''; } };

  function renderSuggest() {
    if (!suggest) return;
    const typed = input.value.trim().toLowerCase();
    const matches = RecentSearches.list.filter(s => !typed || s.toLowerCase().includes(typed));
    if (!matches.length) return hide();
    suggest.hidden = false;
    suggest.innerHTML =
      `<div class="ss-head">Recent searches</div>` +
      matches.map(s => `<button type="button" class="ss-item" data-q="${escapeHtml(s)}"><span class="ss-icon">&#8635;</span><span class="ss-text">${escapeHtml(s)}</span><span class="ss-x" data-rm="${escapeHtml(s)}" title="Remove">&times;</span></button>`).join('') +
      `<button type="button" class="ss-clear">Clear recent searches</button>`;
  }

  input.addEventListener('focus', renderSuggest);
  input.addEventListener('input', renderSuggest);
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { if (input.value) { input.value = ''; renderSuggest(); } else { hide(); input.blur(); } }
  });
  input.addEventListener('blur', () => setTimeout(hide, 150));

  if (suggest) suggest.addEventListener('mousedown', (e) => {
    // mousedown (not click) so it runs before the input's blur tears the panel down;
    // preventDefault keeps focus on the input for the remove/clear cases.
    const rm = e.target.closest('[data-rm]');
    if (rm) { e.preventDefault(); RecentSearches.remove(rm.dataset.rm); renderSuggest(); return; }
    if (e.target.closest('.ss-clear')) { e.preventDefault(); RecentSearches.clear(); hide(); return; }
    const item = e.target.closest('.ss-item');
    if (item) { e.preventDefault(); go(item.dataset.q); }
  });

  form.addEventListener('submit', (e) => { e.preventDefault(); go(input.value); });
}

// Press "/" anywhere to jump to the search box.
document.addEventListener('keydown', (e) => {
  if (e.key !== '/' || e.ctrlKey || e.metaKey || e.altKey) return;
  const t = (e.target && e.target.tagName) || '';
  if (t === 'INPUT' || t === 'TEXTAREA' || t === 'SELECT' || (e.target && e.target.isContentEditable)) return;
  const input = document.getElementById('search-input');
  if (!input) return;
  e.preventDefault();
  input.focus();
  input.select();
});

// Global handlers shared by every page.
document.addEventListener('DOMContentLoaded', () => {
  renderTopbar();
  Notifs.load();
  // check again every minute while the tab is in front
  setInterval(() => { if (!document.hidden) Notifs.load(); }, 60 * 1000);
  document.body.addEventListener('click', (e) => {
    const wl = e.target.closest('[data-watchlater]');
    if (wl) {
      e.preventDefault();
      if (!requireLogin()) return;
      api('/api/me/watch-later', { method: 'POST', body: { videoId: wl.dataset.watchlater } })
        .then(r => toast(r.added ? 'Saved to Watch later' : 'Already in Watch later'))
        .catch(err => toast(err.message, 'error'));
      return;
    }
    const q = e.target.closest('[data-queue]');
    if (q) {
      e.preventDefault();
      if (!requireLogin()) return;
      Queue.add(q.dataset.queue);
      toast('Added to queue');
    }
  });
});
