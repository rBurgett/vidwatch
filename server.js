require('dotenv').config();

const path = require('path');
const fs = require('fs');
const crypto = require('crypto');
const express = require('express');
const bcrypt = require('bcryptjs');
const multer = require('multer');

const db = require('./db');
const { signToken, optionalAuth, requireAuth } = require('./auth');

const PORT = parseInt(process.env.PORT || '5896', 10);
const UPLOADS_DIR = path.resolve(process.env.UPLOADS_DIR || path.join(__dirname, 'uploads'));
const MAX_UPLOAD_MB = parseInt(process.env.MAX_UPLOAD_MB || '500', 10);

for (const sub of ['videos', 'thumbnails', 'avatars', 'banners']) {
  fs.mkdirSync(path.join(UPLOADS_DIR, sub), { recursive: true });
}

const app = express();
// Honor X-Forwarded-For from a reverse proxy (Render, Cloudflare, nginx, etc.)
// so req.ip is the real client IP — matters for unique-viewer counting.
app.set('trust proxy', true);
app.use(express.json({ limit: '1mb' }));
app.use(optionalAuth);

// ---------- Static files ----------
app.use(express.static(path.join(__dirname, 'public')));

// Serve uploaded videos with byte-range support so the browser can seek.
app.get('/media/videos/:filename', (req, res) => {
  const filename = path.basename(req.params.filename);
  const filePath = path.join(UPLOADS_DIR, 'videos', filename);
  if (!fs.existsSync(filePath)) return res.status(404).end();

  const stat = fs.statSync(filePath);
  const range = req.headers.range;
  const ext = path.extname(filename).toLowerCase();
  const contentType = ext === '.webm' ? 'video/webm' : ext === '.mov' ? 'video/quicktime' : 'video/mp4';

  if (!range) {
    res.writeHead(200, { 'Content-Type': contentType, 'Content-Length': stat.size, 'Accept-Ranges': 'bytes' });
    return fs.createReadStream(filePath).pipe(res);
  }

  // Parse "bytes=<start>-<end>" (either side may be omitted), plus the
  // suffix form "bytes=-<n>" (the last n bytes).
  const match = /^bytes=(\d*)-(\d*)$/.exec(String(range).trim());
  let start, end;
  if (match && match[1] === '' && match[2] !== '') {
    end = stat.size - 1;
    start = Math.max(0, stat.size - parseInt(match[2], 10));
  } else {
    start = match && match[1] ? parseInt(match[1], 10) : 0;
    end = match && match[2] ? parseInt(match[2], 10) : stat.size - 1;
    // RFC 7233: an end past EOF is "the remainder of the file", not an error —
    // clamp it. (Browsers routinely overshoot when seeking near the end.)
    if (end >= stat.size) end = stat.size - 1;
  }
  if (!Number.isFinite(start) || !Number.isFinite(end) || start >= stat.size || end < start) {
    res.writeHead(416, { 'Content-Range': `bytes */${stat.size}` });
    return res.end();
  }
  res.writeHead(206, {
    'Content-Type': contentType,
    'Content-Range': `bytes ${start}-${end}/${stat.size}`,
    'Accept-Ranges': 'bytes',
    'Content-Length': end - start + 1
  });
  fs.createReadStream(filePath, { start, end }).pipe(res);
});

app.use('/media/thumbnails', express.static(path.join(UPLOADS_DIR, 'thumbnails'), { maxAge: '7d' }));
app.use('/media/avatars', express.static(path.join(UPLOADS_DIR, 'avatars'), { maxAge: '7d' }));
app.use('/media/banners', express.static(path.join(UPLOADS_DIR, 'banners'), { maxAge: '7d' }));

// ---------- Multer upload config ----------
const storage = multer.diskStorage({
  destination(req, file, cb) {
    const sub = file.fieldname === 'thumbnail' ? 'thumbnails'
      : file.fieldname === 'avatar' ? 'avatars'
      : file.fieldname === 'banner' ? 'banners'
      : 'videos';
    cb(null, path.join(UPLOADS_DIR, sub));
  },
  filename(req, file, cb) {
    const id = crypto.randomBytes(16).toString('hex');
    const ext = path.extname(file.originalname).toLowerCase().slice(0, 8) || '.bin';
    cb(null, `${id}${ext}`);
  }
});

const upload = multer({
  storage,
  limits: { fileSize: MAX_UPLOAD_MB * 1024 * 1024 },
  fileFilter(req, file, cb) {
    if (file.fieldname === 'video') {
      if (!/^video\//.test(file.mimetype)) return cb(new Error('Video file must be a video'));
    } else if (file.fieldname === 'thumbnail' || file.fieldname === 'avatar' || file.fieldname === 'banner') {
      const label = { avatar: 'Avatar', banner: 'Banner', thumbnail: 'Thumbnail' }[file.fieldname];
      if (!/^image\//.test(file.mimetype)) return cb(new Error(`${label} must be an image`));
    }
    cb(null, true);
  }
});

const cleanupUploads = (req) => {
  if (!req.files) return;
  for (const list of Object.values(req.files)) {
    for (const f of list) fs.unlink(f.path, () => {});
  }
};

// Wrap a multer fields() middleware so upload errors become clean 400 JSON
// (and any partially-written files get cleaned up).
const uploadFields = (fields) => (req, res, next) => {
  upload.fields(fields)(req, res, (err) => {
    if (err) {
      cleanupUploads(req);
      return res.status(400).json({ error: err.message });
    }
    next();
  });
};

// ---------- Constants ----------
const RATINGS = ['G', 'PG', 'PG-13', 'R'];
const RATING_RANK = { 'G': 0, 'PG': 1, 'PG-13': 2, 'R': 3 };
const VISIBILITIES = ['public', 'unlisted'];
const PAGE_SIZE = 24;
const TITLE_MAX = 120;
const DESCRIPTION_MAX = 5000;
const BIO_MAX = 500;
const COMMENT_MAX = 2000;
const REPORT_MAX = 500;
const PLAYLIST_TITLE_MAX = 120;
const DAY_MS = 24 * 60 * 60 * 1000;
const SINCE_MS = { '24h': DAY_MS, '7d': 7 * DAY_MS, '30d': 30 * DAY_MS, 'year': 365 * DAY_MS };

// ---------- Helpers ----------
const avatarUrl = (filename) => (filename ? `/media/avatars/${filename}` : null);
const bannerUrl = (filename) => (filename ? `/media/banners/${filename}` : null);

const userPublic = (u) => u && {
  id: u.id,
  username: u.username,
  bio: u.bio || '',
  avatarUrl: avatarUrl(u.avatar_filename),
  bannerUrl: bannerUrl(u.banner_filename),
  playbackSpeed: u.playback_speed != null ? u.playback_speed : 1
};

const videoPublic = (v) => ({
  id: v.id,
  title: v.title,
  description: v.description || '',
  videoUrl: `/media/videos/${v.video_filename}`,
  thumbnailUrl: v.thumbnail_filename ? `/media/thumbnails/${v.thumbnail_filename}` : null,
  views: v.views || 0,
  likes: v.like_count || 0,
  durationSeconds: v.duration_seconds != null ? v.duration_seconds : null,
  ageRating: v.age_rating || 'G',
  visibility: v.visibility || 'public',
  createdAt: v.created_at,
  author: {
    id: v.user_id,
    username: v.author_username,
    avatarUrl: avatarUrl(v.author_avatar)
  }
});

const SELECT_VIDEO_BASE = `
  SELECT v.*, u.username AS author_username, u.avatar_filename AS author_avatar,
    (SELECT COUNT(*) FROM likes l WHERE l.video_id = v.id) AS like_count,
    (SELECT COUNT(*) FROM video_views vv WHERE vv.video_id = v.id) AS views
  FROM videos v
  JOIN users u ON u.id = v.user_id
`;

const parsePage = (req) => Math.max(0, parseInt(req.query.offset || '0', 10) || 0);

// Ratings allowed when "restricted mode" caps at maxRating (e.g. 'PG' -> ['G','PG']).
const allowedRatings = (maxRating) => {
  const max = RATING_RANK[maxRating];
  if (max === undefined) return null;
  return RATINGS.filter((r) => RATING_RANK[r] <= max);
};

const sanitizeFilename = (s) =>
  String(s || 'video').replace(/[^\w.\- ]+/g, '_').replace(/\s+/g, ' ').trim().slice(0, 80) || 'video';

const commentPublic = (r) => ({
  id: r.id,
  body: r.body,
  createdAt: r.created_at,
  editedAt: r.edited_at || null,
  parentId: r.parent_id || null,
  author: { id: r.user_id, username: r.username, avatarUrl: avatarUrl(r.avatar) }
});

// ---------- Auth routes ----------
app.post('/api/auth/register', (req, res) => {
  const { email, username, password } = req.body || {};
  if (!email || !username || !password) return res.status(400).json({ error: 'Email, username, and password are required' });
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) return res.status(400).json({ error: 'Invalid email address' });
  if (!/^[a-zA-Z0-9_]{3,24}$/.test(username)) return res.status(400).json({ error: 'Username must be 3-24 chars, letters/numbers/underscore only' });
  if (password.length < 8) return res.status(400).json({ error: 'Password must be at least 8 characters' });

  const exists = db.prepare('SELECT id FROM users WHERE email = ? OR username = ?').get(email.toLowerCase(), username);
  if (exists) return res.status(409).json({ error: 'Email or username already in use' });

  const hash = bcrypt.hashSync(password, 10);
  const info = db.prepare(
    'INSERT INTO users (email, username, password_hash, created_at) VALUES (?, ?, ?, ?)'
  ).run(email.toLowerCase(), username, hash, Date.now());

  const user = db.prepare('SELECT id, email, username, bio, avatar_filename, banner_filename, playback_speed FROM users WHERE id = ?').get(info.lastInsertRowid);
  res.json({ token: signToken(user), user: userPublic(user) });
});

app.post('/api/auth/login', (req, res) => {
  const { emailOrUsername, password } = req.body || {};
  if (!emailOrUsername || !password) return res.status(400).json({ error: 'Email/username and password required' });
  const user = db.prepare('SELECT * FROM users WHERE email = ? OR username = ?')
    .get(emailOrUsername.toLowerCase(), emailOrUsername);
  if (!user || !bcrypt.compareSync(password, user.password_hash)) {
    return res.status(401).json({ error: 'Invalid credentials' });
  }
  res.json({ token: signToken(user), user: userPublic(user) });
});

app.get('/api/auth/me', (req, res) => {
  if (!req.user) return res.status(401).json({ error: 'Not signed in' });
  res.json({ user: userPublic(req.user) });
});

// ---------- Video list / search / discovery ----------
function buildVideoQuery(req) {
  const where = ['v.visibility = \'public\''];
  const params = [];

  const q = (req.query.q || '').trim();
  if (q) {
    where.push('(v.title LIKE ? OR v.description LIKE ?)');
    params.push(`%${q}%`, `%${q}%`);
  }
  if (req.query.mine === '1' && req.user) {
    where.push('v.user_id = ?');
    params.push(req.user.id);
  }
  if (req.query.following === '1' && req.user) {
    where.push('v.user_id IN (SELECT creator_id FROM subscriptions WHERE follower_id = ?)');
    params.push(req.user.id);
  }
  if (RATINGS.includes(req.query.rating)) {
    where.push('v.age_rating = ?');
    params.push(req.query.rating);
  }
  const allowed = allowedRatings(req.query.maxRating);
  if (allowed) {
    where.push(`v.age_rating IN (${allowed.map(() => '?').join(', ')})`);
    params.push(...allowed);
  }
  if (req.query.duration === 'short') where.push('v.duration_seconds IS NOT NULL AND v.duration_seconds < 240');
  else if (req.query.duration === 'medium') where.push('v.duration_seconds >= 240 AND v.duration_seconds < 1200');
  else if (req.query.duration === 'long') where.push('v.duration_seconds >= 1200');
  if (SINCE_MS[req.query.since]) {
    where.push('v.created_at > ?');
    params.push(Date.now() - SINCE_MS[req.query.since]);
  }

  let order;
  if (req.query.sort === 'views') order = 'ORDER BY views DESC, v.created_at DESC';
  else if (req.query.sort === 'likes') order = 'ORDER BY like_count DESC, v.created_at DESC';
  else order = 'ORDER BY v.created_at DESC';

  const offset = parsePage(req);
  const sql = `${SELECT_VIDEO_BASE} WHERE ${where.join(' AND ')} ${order} LIMIT ? OFFSET ?`;
  return { sql, params: [...params, PAGE_SIZE + 1, offset] };
}

app.get('/api/videos', (req, res) => {
  const { sql, params } = buildVideoQuery(req);
  const rows = db.prepare(sql).all(...params);
  const hasMore = rows.length > PAGE_SIZE;
  if (hasMore) rows.pop();
  res.json({ videos: rows.map(videoPublic), hasMore });
});

app.get('/api/trending', (req, res) => {
  const since = Date.now() - 7 * DAY_MS;
  const where = ['v.visibility = \'public\''];
  const params = [];
  const allowed = allowedRatings(req.query.maxRating);
  if (allowed) {
    where.push(`v.age_rating IN (${allowed.map(() => '?').join(', ')})`);
    params.push(...allowed);
  }
  const offset = parsePage(req);
  const rows = db.prepare(`
    SELECT v.*, u.username AS author_username, u.avatar_filename AS author_avatar,
      (SELECT COUNT(*) FROM likes l WHERE l.video_id = v.id) AS like_count,
      (SELECT COUNT(*) FROM video_views vv WHERE vv.video_id = v.id) AS views,
      (SELECT COUNT(*) FROM video_views vv WHERE vv.video_id = v.id AND vv.created_at > ?) AS recent_views
    FROM videos v JOIN users u ON u.id = v.user_id
    WHERE ${where.join(' AND ')}
    ORDER BY recent_views DESC, views DESC, v.created_at DESC
    LIMIT ? OFFSET ?
  `).all(since, ...params, PAGE_SIZE + 1, offset);
  const hasMore = rows.length > PAGE_SIZE;
  if (hasMore) rows.pop();
  res.json({ videos: rows.map(videoPublic), hasMore });
});

// ---------- Single video ----------
app.get('/api/videos/:id', (req, res) => {
  const id = parseInt(req.params.id, 10);
  if (Number.isNaN(id)) return res.status(400).json({ error: 'Invalid id' });
  const row = db.prepare(`${SELECT_VIDEO_BASE} WHERE v.id = ?`).get(id);
  if (!row) return res.status(404).json({ error: 'Video not found' });

  // Record a unique view: one per signed-in user, otherwise one per IP.
  // Owners watching their own video don't count.
  if (!req.user || req.user.id !== row.user_id) {
    const viewerKey = req.user ? `u:${req.user.id}` : `ip:${req.ip}`;
    const result = db.prepare(
      'INSERT OR IGNORE INTO video_views (video_id, viewer_key, created_at) VALUES (?, ?, ?)'
    ).run(id, viewerKey, Date.now());
    if (result.changes > 0) row.views += 1;
  }

  let resumePosition = 0;
  if (req.user) {
    db.prepare(`
      INSERT INTO watch_history (user_id, video_id, position_seconds, updated_at) VALUES (?, ?, 0, ?)
      ON CONFLICT(user_id, video_id) DO UPDATE SET updated_at = excluded.updated_at
    `).run(req.user.id, id, Date.now());
    const wh = db.prepare('SELECT position_seconds FROM watch_history WHERE user_id = ? AND video_id = ?').get(req.user.id, id);
    resumePosition = wh ? wh.position_seconds : 0;
  }

  const liked = req.user
    ? !!db.prepare('SELECT 1 FROM likes WHERE user_id = ? AND video_id = ?').get(req.user.id, id)
    : false;
  const subscribed = req.user
    ? !!db.prepare('SELECT 1 FROM subscriptions WHERE follower_id = ? AND creator_id = ?').get(req.user.id, row.user_id)
    : false;
  const savedForLater = req.user
    ? !!db.prepare('SELECT 1 FROM watch_later WHERE user_id = ? AND video_id = ?').get(req.user.id, id)
    : false;

  const related = db.prepare(`
    ${SELECT_VIDEO_BASE} WHERE v.id != ? AND v.visibility = 'public'
    ORDER BY (v.user_id = ?) DESC, v.created_at DESC LIMIT 12
  `).all(id, row.user_id).map(videoPublic);

  let reports = [];
  if (req.user && req.user.id === row.user_id) {
    reports = db.prepare(`
      SELECT r.reason, r.created_at, u.username FROM reports r JOIN users u ON u.id = r.reporter_id
      WHERE r.video_id = ? ORDER BY r.created_at DESC
    `).all(id).map((r) => ({ reason: r.reason, createdAt: r.created_at, reporter: r.username }));
  }

  res.json({ video: videoPublic(row), liked, subscribed, savedForLater, related, reports, resumePosition });
});

app.get('/api/videos/:id/download', (req, res) => {
  const id = parseInt(req.params.id, 10);
  const video = db.prepare('SELECT title, video_filename FROM videos WHERE id = ?').get(id);
  if (!video) return res.status(404).json({ error: 'Video not found' });
  const filePath = path.join(UPLOADS_DIR, 'videos', video.video_filename);
  if (!fs.existsSync(filePath)) return res.status(404).json({ error: 'File missing' });
  const ext = path.extname(video.video_filename) || '.mp4';
  res.download(filePath, `${sanitizeFilename(video.title)}${ext}`);
});

// ---------- Watch history ----------
app.post('/api/videos/:id/progress', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const video = db.prepare('SELECT id, duration_seconds FROM videos WHERE id = ?').get(id);
  if (!video) return res.status(404).json({ error: 'Not found' });
  let pos = parseFloat(req.body && req.body.position);
  if (!Number.isFinite(pos) || pos < 0) pos = 0;
  if (video.duration_seconds && pos > video.duration_seconds) pos = video.duration_seconds;
  db.prepare(`
    INSERT INTO watch_history (user_id, video_id, position_seconds, updated_at) VALUES (?, ?, ?, ?)
    ON CONFLICT(user_id, video_id) DO UPDATE SET position_seconds = excluded.position_seconds, updated_at = excluded.updated_at
  `).run(req.user.id, id, pos, Date.now());
  res.json({ ok: true });
});

app.get('/api/me/history', requireAuth, (req, res) => {
  const offset = parsePage(req);
  const rows = db.prepare(`
    SELECT v.*, u.username AS author_username, u.avatar_filename AS author_avatar,
      (SELECT COUNT(*) FROM likes l WHERE l.video_id = v.id) AS like_count,
      (SELECT COUNT(*) FROM video_views vv WHERE vv.video_id = v.id) AS views,
      wh.position_seconds AS history_position, wh.updated_at AS history_updated_at
    FROM watch_history wh
    JOIN videos v ON v.id = wh.video_id
    JOIN users u ON u.id = v.user_id
    WHERE wh.user_id = ?
    ORDER BY wh.updated_at DESC LIMIT ? OFFSET ?
  `).all(req.user.id, PAGE_SIZE + 1, offset);
  const hasMore = rows.length > PAGE_SIZE;
  if (hasMore) rows.pop();
  res.json({
    items: rows.map((r) => ({ video: videoPublic(r), position: r.history_position, updatedAt: r.history_updated_at })),
    hasMore
  });
});

app.delete('/api/me/history', requireAuth, (req, res) => {
  db.prepare('DELETE FROM watch_history WHERE user_id = ?').run(req.user.id);
  res.json({ ok: true });
});

app.delete('/api/me/history/:videoId', requireAuth, (req, res) => {
  db.prepare('DELETE FROM watch_history WHERE user_id = ? AND video_id = ?').run(req.user.id, parseInt(req.params.videoId, 10));
  res.json({ ok: true });
});

// ---------- Watch later ----------
app.get('/api/me/watch-later', requireAuth, (req, res) => {
  const offset = parsePage(req);
  const rows = db.prepare(`
    ${SELECT_VIDEO_BASE} JOIN watch_later wl ON wl.video_id = v.id
    WHERE wl.user_id = ?
    ORDER BY wl.added_at DESC LIMIT ? OFFSET ?
  `).all(req.user.id, PAGE_SIZE + 1, offset);
  const hasMore = rows.length > PAGE_SIZE;
  if (hasMore) rows.pop();
  res.json({ videos: rows.map(videoPublic), hasMore });
});

app.post('/api/me/watch-later', requireAuth, (req, res) => {
  const videoId = parseInt(req.body && req.body.videoId, 10);
  if (Number.isNaN(videoId)) return res.status(400).json({ error: 'Invalid video id' });
  const video = db.prepare('SELECT id FROM videos WHERE id = ?').get(videoId);
  if (!video) return res.status(404).json({ error: 'Video not found' });
  const info = db.prepare('INSERT OR IGNORE INTO watch_later (user_id, video_id, added_at) VALUES (?, ?, ?)')
    .run(req.user.id, videoId, Date.now());
  res.json({ ok: true, saved: true, added: info.changes > 0 });
});

app.delete('/api/me/watch-later', requireAuth, (req, res) => {
  db.prepare('DELETE FROM watch_later WHERE user_id = ?').run(req.user.id);
  res.json({ ok: true });
});

app.delete('/api/me/watch-later/:videoId', requireAuth, (req, res) => {
  db.prepare('DELETE FROM watch_later WHERE user_id = ? AND video_id = ?').run(req.user.id, parseInt(req.params.videoId, 10));
  res.json({ ok: true });
});

// ---------- Notifications ----------
// Derived, not stored: public uploads from creators you've saved, posted after
// you saved them (so saving a creator doesn't dump their whole back catalog on you).
const NOTIFICATIONS_MAX = 30;

app.get('/api/me/notifications', requireAuth, (req, res) => {
  const seenAt = (db.prepare('SELECT notifications_seen_at FROM users WHERE id = ?').get(req.user.id) || {}).notifications_seen_at || 0;
  const rows = db.prepare(`
    ${SELECT_VIDEO_BASE} JOIN subscriptions s ON s.creator_id = v.user_id AND s.follower_id = ?
    WHERE v.visibility = 'public' AND v.created_at > s.created_at
    ORDER BY v.created_at DESC LIMIT ?
  `).all(req.user.id, NOTIFICATIONS_MAX);
  const items = rows.map((r) => ({ video: videoPublic(r), unread: r.created_at > seenAt }));
  res.json({ items, unread: items.filter((it) => it.unread).length });
});

app.post('/api/me/notifications/seen', requireAuth, (req, res) => {
  db.prepare('UPDATE users SET notifications_seen_at = ? WHERE id = ?').run(Date.now(), req.user.id);
  res.json({ ok: true });
});

// ---------- Upload / edit / delete ----------
app.post('/api/videos', requireAuth, uploadFields([
  { name: 'video', maxCount: 1 },
  { name: 'thumbnail', maxCount: 1 }
]), (req, res) => {
  const videoFile = req.files && req.files.video && req.files.video[0];
  const thumbFile = req.files && req.files.thumbnail && req.files.thumbnail[0];
  const fail = (msg) => { cleanupUploads(req); res.status(400).json({ error: msg }); };

  if (!videoFile) return fail('A video file is required');

  const title = (req.body.title || '').trim();
  const description = (req.body.description || '').trim();
  if (!title) return fail('Title is required');
  if (title.length > TITLE_MAX) return fail(`Title must be ${TITLE_MAX} characters or fewer`);
  if (description.length > DESCRIPTION_MAX) return fail(`Description must be ${DESCRIPTION_MAX} characters or fewer`);

  const ageRating = RATINGS.includes(req.body.ageRating) ? req.body.ageRating : 'G';
  const visibility = VISIBILITIES.includes(req.body.visibility) ? req.body.visibility : 'public';
  let durationSeconds = parseFloat(req.body.durationSeconds);
  if (!Number.isFinite(durationSeconds) || durationSeconds <= 0 || durationSeconds > 24 * 3600) durationSeconds = null;

  const info = db.prepare(`
    INSERT INTO videos (user_id, title, description, video_filename, thumbnail_filename, mime_type, duration_seconds, age_rating, visibility, created_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  `).run(
    req.user.id, title, description,
    path.basename(videoFile.path),
    thumbFile ? path.basename(thumbFile.path) : null,
    videoFile.mimetype, durationSeconds, ageRating, visibility, Date.now()
  );

  const row = db.prepare(`${SELECT_VIDEO_BASE} WHERE v.id = ?`).get(info.lastInsertRowid);
  res.json({ video: videoPublic(row) });
});

app.patch('/api/videos/:id', requireAuth, uploadFields([{ name: 'thumbnail', maxCount: 1 }]), (req, res) => {
  const id = parseInt(req.params.id, 10);
  const video = db.prepare('SELECT * FROM videos WHERE id = ?').get(id);
  const fail = (code, msg) => { cleanupUploads(req); res.status(code).json({ error: msg }); };
  if (!video) return fail(404, 'Not found');
  if (video.user_id !== req.user.id) return fail(403, 'Not your video');

  const title = (req.body.title !== undefined ? String(req.body.title) : video.title).trim();
  const description = (req.body.description !== undefined ? String(req.body.description) : (video.description || '')).trim();
  if (!title) return fail(400, 'Title is required');
  if (title.length > TITLE_MAX) return fail(400, `Title must be ${TITLE_MAX} characters or fewer`);
  if (description.length > DESCRIPTION_MAX) return fail(400, `Description must be ${DESCRIPTION_MAX} characters or fewer`);

  const ageRating = RATINGS.includes(req.body.ageRating) ? req.body.ageRating : video.age_rating;
  const visibility = VISIBILITIES.includes(req.body.visibility) ? req.body.visibility : video.visibility;
  const thumbFile = req.files && req.files.thumbnail && req.files.thumbnail[0];
  const thumbName = thumbFile ? path.basename(thumbFile.path) : video.thumbnail_filename;

  db.prepare('UPDATE videos SET title = ?, description = ?, age_rating = ?, visibility = ?, thumbnail_filename = ? WHERE id = ?')
    .run(title, description, ageRating, visibility, thumbName, id);
  if (thumbFile && video.thumbnail_filename) {
    fs.unlink(path.join(UPLOADS_DIR, 'thumbnails', video.thumbnail_filename), () => {});
  }
  const row = db.prepare(`${SELECT_VIDEO_BASE} WHERE v.id = ?`).get(id);
  res.json({ video: videoPublic(row) });
});

app.delete('/api/videos/:id', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const video = db.prepare('SELECT * FROM videos WHERE id = ?').get(id);
  if (!video) return res.status(404).json({ error: 'Not found' });
  if (video.user_id !== req.user.id) return res.status(403).json({ error: 'Not your video' });

  db.prepare('DELETE FROM videos WHERE id = ?').run(id);
  fs.unlink(path.join(UPLOADS_DIR, 'videos', video.video_filename), () => {});
  if (video.thumbnail_filename) fs.unlink(path.join(UPLOADS_DIR, 'thumbnails', video.thumbnail_filename), () => {});
  res.json({ ok: true });
});

// ---------- Likes ----------
app.post('/api/videos/:id/like', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const video = db.prepare('SELECT id FROM videos WHERE id = ?').get(id);
  if (!video) return res.status(404).json({ error: 'Not found' });

  const existing = db.prepare('SELECT 1 FROM likes WHERE user_id = ? AND video_id = ?').get(req.user.id, id);
  if (existing) {
    db.prepare('DELETE FROM likes WHERE user_id = ? AND video_id = ?').run(req.user.id, id);
  } else {
    db.prepare('INSERT INTO likes (user_id, video_id, created_at) VALUES (?, ?, ?)').run(req.user.id, id, Date.now());
  }
  const count = db.prepare('SELECT COUNT(*) AS c FROM likes WHERE video_id = ?').get(id).c;
  res.json({ liked: !existing, likes: count });
});

// ---------- Comments ----------
app.get('/api/videos/:id/comments', (req, res) => {
  const id = parseInt(req.params.id, 10);
  const dir = req.query.sort === 'old' ? 'ASC' : 'DESC';
  const rows = db.prepare(`
    SELECT c.id, c.body, c.created_at, c.edited_at, c.parent_id, u.id AS user_id, u.username, u.avatar_filename AS avatar
    FROM comments c JOIN users u ON u.id = c.user_id
    WHERE c.video_id = ? ORDER BY c.created_at ${dir} LIMIT 500
  `).all(id);
  res.json({ comments: rows.map(commentPublic) });
});

app.post('/api/videos/:id/comments', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const body = (req.body.body || '').trim();
  if (!body) return res.status(400).json({ error: 'Comment cannot be empty' });
  if (body.length > COMMENT_MAX) return res.status(400).json({ error: 'Comment too long' });
  const video = db.prepare('SELECT id FROM videos WHERE id = ?').get(id);
  if (!video) return res.status(404).json({ error: 'Not found' });

  let parentId = null;
  if (req.body.parentId != null && req.body.parentId !== '') {
    parentId = parseInt(req.body.parentId, 10);
    const parent = db.prepare('SELECT id, video_id, parent_id FROM comments WHERE id = ?').get(parentId);
    if (!parent || parent.video_id !== id) return res.status(400).json({ error: 'Parent comment not found' });
    if (parent.parent_id != null) return res.status(400).json({ error: 'Cannot reply to a reply' });
  }

  const info = db.prepare('INSERT INTO comments (user_id, video_id, parent_id, body, created_at) VALUES (?, ?, ?, ?, ?)')
    .run(req.user.id, id, parentId, body, Date.now());
  const row = db.prepare(`
    SELECT c.id, c.body, c.created_at, c.edited_at, c.parent_id, u.id AS user_id, u.username, u.avatar_filename AS avatar
    FROM comments c JOIN users u ON u.id = c.user_id WHERE c.id = ?
  `).get(info.lastInsertRowid);
  res.json({ comment: commentPublic(row) });
});

app.patch('/api/videos/:id/comments/:commentId', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const commentId = parseInt(req.params.commentId, 10);
  const body = (req.body.body || '').trim();
  if (!body) return res.status(400).json({ error: 'Comment cannot be empty' });
  if (body.length > COMMENT_MAX) return res.status(400).json({ error: 'Comment too long' });
  const comment = db.prepare('SELECT * FROM comments WHERE id = ?').get(commentId);
  if (!comment || comment.video_id !== id) return res.status(404).json({ error: 'Not found' });
  if (comment.user_id !== req.user.id) return res.status(403).json({ error: 'Not your comment' });
  db.prepare('UPDATE comments SET body = ?, edited_at = ? WHERE id = ?').run(body, Date.now(), commentId);
  const row = db.prepare(`
    SELECT c.id, c.body, c.created_at, c.edited_at, c.parent_id, u.id AS user_id, u.username, u.avatar_filename AS avatar
    FROM comments c JOIN users u ON u.id = c.user_id WHERE c.id = ?
  `).get(commentId);
  res.json({ comment: commentPublic(row) });
});

app.delete('/api/videos/:id/comments/:commentId', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const commentId = parseInt(req.params.commentId, 10);
  const comment = db.prepare('SELECT c.*, v.user_id AS video_owner FROM comments c JOIN videos v ON v.id = c.video_id WHERE c.id = ?').get(commentId);
  if (!comment || comment.video_id !== id) return res.status(404).json({ error: 'Not found' });
  if (comment.user_id !== req.user.id && comment.video_owner !== req.user.id) {
    return res.status(403).json({ error: 'Not allowed' });
  }
  db.prepare('DELETE FROM comments WHERE id = ?').run(commentId);
  res.json({ ok: true });
});

// ---------- Reports ----------
app.post('/api/videos/:id/report', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const reason = (req.body.reason || '').trim();
  if (!reason) return res.status(400).json({ error: 'Please describe the problem' });
  if (reason.length > REPORT_MAX) return res.status(400).json({ error: 'Report too long' });
  const video = db.prepare('SELECT id FROM videos WHERE id = ?').get(id);
  if (!video) return res.status(404).json({ error: 'Not found' });
  const info = db.prepare('INSERT OR IGNORE INTO reports (video_id, reporter_id, reason, created_at) VALUES (?, ?, ?, ?)')
    .run(id, req.user.id, reason, Date.now());
  res.json({ ok: true, alreadyReported: info.changes === 0 });
});

// ---------- Subscriptions ----------
app.post('/api/users/:username/subscribe', requireAuth, (req, res) => {
  const creator = db.prepare('SELECT id FROM users WHERE username = ?').get(req.params.username);
  if (!creator) return res.status(404).json({ error: 'User not found' });
  if (creator.id === req.user.id) return res.status(400).json({ error: "You can't save yourself" });
  const existing = db.prepare('SELECT 1 FROM subscriptions WHERE follower_id = ? AND creator_id = ?').get(req.user.id, creator.id);
  if (existing) {
    db.prepare('DELETE FROM subscriptions WHERE follower_id = ? AND creator_id = ?').run(req.user.id, creator.id);
  } else {
    db.prepare('INSERT INTO subscriptions (follower_id, creator_id, created_at) VALUES (?, ?, ?)').run(req.user.id, creator.id, Date.now());
  }
  const subscribers = db.prepare('SELECT COUNT(*) AS c FROM subscriptions WHERE creator_id = ?').get(creator.id).c;
  res.json({ subscribed: !existing, subscribers });
});

// ---------- User profiles & settings ----------
app.get('/api/users/:username', (req, res) => {
  const user = db.prepare('SELECT id, username, bio, avatar_filename, banner_filename, created_at FROM users WHERE username = ?').get(req.params.username);
  if (!user) return res.status(404).json({ error: 'User not found' });
  const isSelf = !!(req.user && req.user.id === user.id);
  const offset = parsePage(req);
  const visClause = isSelf ? '' : "AND v.visibility = 'public'";
  const videos = db.prepare(`${SELECT_VIDEO_BASE} WHERE v.user_id = ? ${visClause} ORDER BY v.created_at DESC LIMIT ? OFFSET ?`)
    .all(user.id, PAGE_SIZE + 1, offset);
  const hasMore = videos.length > PAGE_SIZE;
  if (hasMore) videos.pop();
  const subscribers = db.prepare('SELECT COUNT(*) AS c FROM subscriptions WHERE creator_id = ?').get(user.id).c;
  const subscribed = req.user ? !!db.prepare('SELECT 1 FROM subscriptions WHERE follower_id = ? AND creator_id = ?').get(req.user.id, user.id) : false;
  res.json({
    user: {
      id: user.id, username: user.username, bio: user.bio || '',
      avatarUrl: avatarUrl(user.avatar_filename), bannerUrl: bannerUrl(user.banner_filename),
      createdAt: user.created_at, subscribers, subscribed, isSelf
    },
    videos: videos.map(videoPublic),
    hasMore
  });
});

app.get('/api/users/:username/liked', requireAuth, (req, res) => {
  const user = db.prepare('SELECT id FROM users WHERE username = ?').get(req.params.username);
  if (!user) return res.status(404).json({ error: 'User not found' });
  if (user.id !== req.user.id) return res.status(403).json({ error: 'Not allowed' });
  const offset = parsePage(req);
  const rows = db.prepare(`
    ${SELECT_VIDEO_BASE} JOIN likes lk ON lk.video_id = v.id
    WHERE lk.user_id = ? AND v.visibility = 'public'
    ORDER BY lk.created_at DESC LIMIT ? OFFSET ?
  `).all(user.id, PAGE_SIZE + 1, offset);
  const hasMore = rows.length > PAGE_SIZE;
  if (hasMore) rows.pop();
  res.json({ videos: rows.map(videoPublic), hasMore });
});

app.patch('/api/users/me', requireAuth, uploadFields([{ name: 'avatar', maxCount: 1 }, { name: 'banner', maxCount: 1 }]), (req, res) => {
  const before = db.prepare('SELECT avatar_filename, banner_filename FROM users WHERE id = ?').get(req.user.id);
  const updates = [];
  const params = [];
  const fail = (msg) => { cleanupUploads(req); res.status(400).json({ error: msg }); };

  if (req.body.bio !== undefined) {
    const bio = String(req.body.bio).trim();
    if (bio.length > BIO_MAX) return fail(`Bio must be ${BIO_MAX} characters or fewer`);
    updates.push('bio = ?'); params.push(bio);
  }
  if (req.body.playbackSpeed !== undefined) {
    let s = parseFloat(req.body.playbackSpeed);
    if (!Number.isFinite(s)) s = 1;
    s = Math.min(3, Math.max(0.25, s));
    updates.push('playback_speed = ?'); params.push(s);
  }
  const avatarFile = req.files && req.files.avatar && req.files.avatar[0];
  if (avatarFile) { updates.push('avatar_filename = ?'); params.push(path.basename(avatarFile.path)); }
  const bannerFile = req.files && req.files.banner && req.files.banner[0];
  if (bannerFile) { updates.push('banner_filename = ?'); params.push(path.basename(bannerFile.path)); }
  // Clearing the banner is its own flag — an empty file field can't express "remove".
  const removeBanner = !bannerFile && String(req.body.removeBanner || '') === 'true';
  if (removeBanner) updates.push('banner_filename = NULL');

  if (updates.length) {
    params.push(req.user.id);
    db.prepare(`UPDATE users SET ${updates.join(', ')} WHERE id = ?`).run(...params);
  }
  if (avatarFile && before && before.avatar_filename) {
    fs.unlink(path.join(UPLOADS_DIR, 'avatars', before.avatar_filename), () => {});
  }
  if ((bannerFile || removeBanner) && before && before.banner_filename) {
    fs.unlink(path.join(UPLOADS_DIR, 'banners', before.banner_filename), () => {});
  }
  const updated = db.prepare('SELECT id, email, username, bio, avatar_filename, banner_filename, playback_speed FROM users WHERE id = ?').get(req.user.id);
  res.json({ user: userPublic(updated) });
});

// ---------- Playlists ----------
const playlistSummary = (p) => ({ id: p.id, title: p.title, createdAt: p.created_at, count: p.count, contains: p.contains != null ? !!p.contains : undefined });

app.get('/api/playlists', requireAuth, (req, res) => {
  const videoId = req.query.videoId ? parseInt(req.query.videoId, 10) : null;
  const rows = db.prepare(`
    SELECT p.id, p.title, p.created_at,
      (SELECT COUNT(*) FROM playlist_items pi WHERE pi.playlist_id = p.id) AS count
      ${videoId ? ', (SELECT 1 FROM playlist_items pi2 WHERE pi2.playlist_id = p.id AND pi2.video_id = ?) AS contains' : ''}
    FROM playlists p WHERE p.user_id = ? ORDER BY p.created_at DESC
  `).all(...(videoId ? [videoId, req.user.id] : [req.user.id]));
  res.json({ playlists: rows.map(playlistSummary) });
});

app.post('/api/playlists', requireAuth, (req, res) => {
  const title = (req.body.title || '').trim();
  if (!title) return res.status(400).json({ error: 'Playlist needs a name' });
  if (title.length > PLAYLIST_TITLE_MAX) return res.status(400).json({ error: 'Name too long' });
  const info = db.prepare('INSERT INTO playlists (user_id, title, created_at) VALUES (?, ?, ?)').run(req.user.id, title, Date.now());
  res.json({ playlist: { id: info.lastInsertRowid, title, createdAt: Date.now(), count: 0 } });
});

app.get('/api/playlists/:id', (req, res) => {
  const id = parseInt(req.params.id, 10);
  const playlist = db.prepare('SELECT p.id, p.title, p.created_at, p.user_id, u.username FROM playlists p JOIN users u ON u.id = p.user_id WHERE p.id = ?').get(id);
  if (!playlist) return res.status(404).json({ error: 'Playlist not found' });
  const isOwner = !!(req.user && req.user.id === playlist.user_id);
  const visClause = isOwner ? '' : "AND v.visibility = 'public'";
  const items = db.prepare(`
    ${SELECT_VIDEO_BASE} JOIN playlist_items pi ON pi.video_id = v.id
    WHERE pi.playlist_id = ? ${visClause} ORDER BY pi.position
  `).all(id).map(videoPublic);
  res.json({
    playlist: { id: playlist.id, title: playlist.title, createdAt: playlist.created_at, owner: { id: playlist.user_id, username: playlist.username }, isOwner },
    items
  });
});

app.post('/api/playlists/:id/items', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const videoId = parseInt(req.body.videoId, 10);
  const playlist = db.prepare('SELECT * FROM playlists WHERE id = ?').get(id);
  if (!playlist) return res.status(404).json({ error: 'Playlist not found' });
  if (playlist.user_id !== req.user.id) return res.status(403).json({ error: 'Not your playlist' });
  const video = db.prepare('SELECT id FROM videos WHERE id = ?').get(videoId);
  if (!video) return res.status(404).json({ error: 'Video not found' });
  const nextPos = db.prepare('SELECT COALESCE(MAX(position), 0) + 1 AS p FROM playlist_items WHERE playlist_id = ?').get(id).p;
  db.prepare('INSERT OR IGNORE INTO playlist_items (playlist_id, video_id, position, added_at) VALUES (?, ?, ?, ?)').run(id, videoId, nextPos, Date.now());
  const count = db.prepare('SELECT COUNT(*) AS c FROM playlist_items WHERE playlist_id = ?').get(id).c;
  res.json({ ok: true, count });
});

app.delete('/api/playlists/:id/items/:videoId', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const playlist = db.prepare('SELECT * FROM playlists WHERE id = ?').get(id);
  if (!playlist) return res.status(404).json({ error: 'Playlist not found' });
  if (playlist.user_id !== req.user.id) return res.status(403).json({ error: 'Not your playlist' });
  db.prepare('DELETE FROM playlist_items WHERE playlist_id = ? AND video_id = ?').run(id, parseInt(req.params.videoId, 10));
  res.json({ ok: true });
});

app.delete('/api/playlists/:id', requireAuth, (req, res) => {
  const id = parseInt(req.params.id, 10);
  const playlist = db.prepare('SELECT * FROM playlists WHERE id = ?').get(id);
  if (!playlist) return res.status(404).json({ error: 'Playlist not found' });
  if (playlist.user_id !== req.user.id) return res.status(403).json({ error: 'Not your playlist' });
  db.prepare('DELETE FROM playlists WHERE id = ?').run(id);
  res.json({ ok: true });
});

// ---------- HTML routes (single-page-ish) ----------
const sendPage = (file) => (_req, res) => res.sendFile(path.join(__dirname, 'public', file));
app.get('/', sendPage('index.html'));
app.get('/login', sendPage('login.html'));
app.get('/register', sendPage('register.html'));
app.get('/upload', sendPage('upload.html'));
app.get('/watch', sendPage('watch.html'));
app.get('/trending', sendPage('trending.html'));
app.get('/library', sendPage('library.html'));
app.get('/playlist', sendPage('playlist.html'));
app.get('/settings', sendPage('settings.html'));
app.get('/u/:username', sendPage('profile.html'));

// ---------- Error handler ----------
app.use((err, _req, res, _next) => {
  // eslint-disable-next-line no-console
  console.error(err);
  if (res.headersSent) return;
  res.status(500).json({ error: err.message || 'Server error' });
});

app.listen(PORT, () => {
  // eslint-disable-next-line no-console
  console.log(`vidwatch listening on http://localhost:${PORT}`);
});
