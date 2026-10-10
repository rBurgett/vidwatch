const path = require('path');
const fs = require('fs');
const Database = require('better-sqlite3');

const dataDir = process.env.DATA_DIR || path.join(__dirname, 'data');
fs.mkdirSync(dataDir, { recursive: true });

const db = new Database(path.join(dataDir, 'vidwatch.db'));
db.pragma('journal_mode = WAL');
db.pragma('foreign_keys = ON');

// This app is "fresh start" oriented — there's no migration path. If the
// database was created by an older version (e.g. `videos` exists but has no
// `visibility` column), bail with a clear hint instead of letting a later
// `CREATE INDEX ... ON videos(visibility)` throw a cryptic "no such column".
{
  const tables = new Set(db.prepare("SELECT name FROM sqlite_master WHERE type='table'").all().map((t) => t.name));
  const colsOf = (t) => new Set(db.prepare(`PRAGMA table_info('${t}')`).all().map((c) => c.name));
  const stale =
    (tables.has('videos') && !colsOf('videos').has('visibility')) ||
    (tables.has('users') && !colsOf('users').has('avatar_filename'));
  if (stale) {
    throw new Error(
      'vidwatch database schema is out of date. Run `npm run reset` to wipe it (and the old uploads), then start the server again.'
    );
  }
}

db.exec(`
  CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL UNIQUE,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    bio TEXT DEFAULT '',
    avatar_filename TEXT,
    banner_filename TEXT,
    playback_speed REAL NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL
  );

  CREATE TABLE IF NOT EXISTS videos (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    description TEXT DEFAULT '',
    video_filename TEXT NOT NULL,
    thumbnail_filename TEXT,
    mime_type TEXT NOT NULL,
    duration_seconds REAL,
    age_rating TEXT NOT NULL DEFAULT 'G',          -- G | PG | PG-13 | R, set by the creator
    visibility TEXT NOT NULL DEFAULT 'public',     -- public | unlisted
    created_at INTEGER NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_videos_user ON videos(user_id);
  CREATE INDEX IF NOT EXISTS idx_videos_created ON videos(created_at DESC);
  CREATE INDEX IF NOT EXISTS idx_videos_visibility ON videos(visibility);

  -- One row per unique viewer per video. viewer_key is "u:<user_id>" for
  -- signed-in viewers and "ip:<ip>" for anonymous viewers, so the same person
  -- watching twice doesn't double-count. created_at is also used to rank
  -- "trending" by recent views.
  CREATE TABLE IF NOT EXISTS video_views (
    video_id INTEGER NOT NULL,
    viewer_key TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (video_id, viewer_key),
    FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_video_views_recent ON video_views(created_at);

  CREATE TABLE IF NOT EXISTS likes (
    user_id INTEGER NOT NULL,
    video_id INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, video_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_likes_user ON likes(user_id, created_at DESC);

  -- parent_id NULL = a top-level comment; otherwise it's a reply. Only one
  -- level of nesting is allowed (enforced in the API). Deleting a top-level
  -- comment cascades its replies.
  CREATE TABLE IF NOT EXISTS comments (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    video_id INTEGER NOT NULL,
    parent_id INTEGER,
    body TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    edited_at INTEGER,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE,
    FOREIGN KEY (parent_id) REFERENCES comments(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_comments_video ON comments(video_id, parent_id, created_at);

  CREATE TABLE IF NOT EXISTS subscriptions (
    follower_id INTEGER NOT NULL,
    creator_id INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (follower_id, creator_id),
    FOREIGN KEY (follower_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (creator_id) REFERENCES users(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_subscriptions_creator ON subscriptions(creator_id);

  -- Server-side watch history: most-recent position per (user, video).
  CREATE TABLE IF NOT EXISTS watch_history (
    user_id INTEGER NOT NULL,
    video_id INTEGER NOT NULL,
    position_seconds REAL NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, video_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_watch_history_recent ON watch_history(user_id, updated_at DESC);

  CREATE TABLE IF NOT EXISTS playlists (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL,
    title TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_playlists_user ON playlists(user_id, created_at DESC);

  CREATE TABLE IF NOT EXISTS playlist_items (
    playlist_id INTEGER NOT NULL,
    video_id INTEGER NOT NULL,
    position INTEGER NOT NULL,
    added_at INTEGER NOT NULL,
    PRIMARY KEY (playlist_id, video_id),
    FOREIGN KEY (playlist_id) REFERENCES playlists(id) ON DELETE CASCADE,
    FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_playlist_items_order ON playlist_items(playlist_id, position);

  CREATE TABLE IF NOT EXISTS reports (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    video_id INTEGER NOT NULL,
    reporter_id INTEGER NOT NULL,
    reason TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (video_id, reporter_id),
    FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE,
    FOREIGN KEY (reporter_id) REFERENCES users(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_reports_video ON reports(video_id);

  -- "Watch later": a per-user save list, ordered by when it was added.
  CREATE TABLE IF NOT EXISTS watch_later (
    user_id INTEGER NOT NULL,
    video_id INTEGER NOT NULL,
    added_at INTEGER NOT NULL,
    PRIMARY KEY (user_id, video_id),
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE
  );
  CREATE INDEX IF NOT EXISTS idx_watch_later ON watch_later(user_id, added_at DESC);
`);

// A nullable column added after the fact can just be tacked on, so databases
// created before profile banners existed keep working without a reset.
{
  const cols = new Set(db.prepare("PRAGMA table_info('users')").all().map((c) => c.name));
  if (!cols.has('banner_filename')) db.exec('ALTER TABLE users ADD COLUMN banner_filename TEXT');
  // last time the user opened the notifications panel; uploads after this are unread
  if (!cols.has('notifications_seen_at')) db.exec('ALTER TABLE users ADD COLUMN notifications_seen_at INTEGER');
}

module.exports = db;
