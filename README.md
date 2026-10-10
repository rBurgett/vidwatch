# vidwatch

A YouTube-like video sharing app: signup/login, upload videos, browse a thumbnail grid, watch with seeking, like, comment, follow creators, build playlists, and more. Built with vanilla HTML/CSS/JS on the frontend and Node.js + Express + SQLite on the backend — no build step.

## Run locally

```bash
npm install
cp .env.example .env       # then edit JWT_SECRET to anything random
npm start
```

Open http://localhost:5896 (or whatever `PORT` you set).

### Start fresh

```bash
npm run reset
```

Deletes the local SQLite database and every uploaded video, thumbnail, and avatar — back to a clean slate. The server re-creates the DB and upload folders on the next boot. (`data/` and `uploads/` are gitignored, so this only touches your local copy.)

## What's in the box

- **Auth & profiles**: email/username + bcrypt-hashed password, JWT in localStorage; profile pages with avatar upload, bio, follower counts, and a "Liked videos" tab.
- **Videos**: drag-and-drop upload (video + optional thumbnail), client-measured duration, HTML5 player with HTTP byte-range streaming, edit (title/description/thumbnail/rating/visibility), delete, and a download button.
- **Age ratings**: every video is rated **G / PG / PG-13 / R** by its creator; the rating shows as a badge everywhere.
- **Kids mode** (per browser): hide anything above a chosen rating across the whole site; while it's on, the "More videos" list, autoplay, and the search box are turned off, and a watch-page age gate blocks higher-rated videos.
- **Player upgrades**: keyboard shortcuts (press `?` for the list), resume where you left off (synced to your account when signed in), playback-speed menu (remembered on your account), theater mode, picture-in-picture, loop, remembered volume.
- **Social**: likes, threaded comments (one reply level) with edit/delete and newest/oldest sorting, follow/unfollow creators, report a video (the uploader sees reports), share menu with "copy link" and "copy link at the current time".
- **Discovery**: search with rating/duration/date filters (press <kbd>/</kbd> anywhere to jump to the box; a recent-searches dropdown remembers your last few queries), a Trending page (most views in the last 7 days), "Continue watching" and "From creators you follow" rows on the home page, and "Load more" pagination.
- **Watch later**: a "+ Later" button on every video thumbnail (and in the watch-page Share menu) saves to your **Watch later** list in the Library — with "Play all" and "Clear all".
- **Playlists & queue**: create playlists, add/remove videos, "Play all" with auto-advance *within the playlist*; a session play queue with the same auto-advance. Recommendations never autoplay.
- **Library**: a single page with your History (with resume + clear), Watch later, Liked videos, Playlists, and Your videos.
- **Appearance**: Light / Dark / System theme toggle (per browser), and `prefers-reduced-motion` is honored automatically.

## File layout

```
server.js           # Express app: routes, range-streaming, multer uploads
db.js               # SQLite schema & connection
auth.js             # JWT sign / verify, requireAuth middleware
scripts/reset.js    # `npm run reset` — wipe the DB + uploaded media
public/             # Static frontend (HTML + CSS + JS, no build step)
data/               # SQLite DB lives here (gitignored)
uploads/            # Uploaded videos / thumbnails / avatars (gitignored)
```

## Deploying

This app is local-first and deploy-ready. To put it online:

1. **Pick a host with a persistent disk** (small VPS, Render with a persistent disk on `/data` and `/uploads`, Fly.io with a mounted volume). Avoid ephemeral-filesystem hosts — uploads vanish on restart.
2. **Set environment variables**:
   - `JWT_SECRET` (required) — long random string. Generate with `node -e "console.log(require('crypto').randomBytes(48).toString('hex'))"`.
   - `PORT` — usually set automatically by the host (defaults to 5896 locally).
   - `DATA_DIR`, `UPLOADS_DIR` — point these at the persistent disk paths.
   - `MAX_UPLOAD_MB` — defaults to 500.
3. **Point your domain at the host** and use the host's built-in TLS (or Cloudflare/Caddy in front of a VPS) so the site is HTTPS.
4. **Run** `npm install --omit=dev && npm start`. The server creates the DB and upload folders on first boot.

## Roadmap (not built yet)

- Recommendations beyond newest-first / trending
- Auto-generated thumbnails (would need ffmpeg)
- Email verification + password reset, change-password / delete-account
- Captions / subtitles
- Adaptive bitrate / HLS streaming for large videos
- Move uploads to S3/R2 once disk usage matters
