# vidwatch architecture

```
vidwatch.burgett.dev        CloudFront ── default ──> S3 site bucket (web/)
                                       └─ /api/* ───> API Gateway HTTP API ──> Go Lambda ──> DynamoDB
media.vidwatch.burgett.dev  CloudFront ─────────────> S3 media bucket
Browser ── presigned POST ─────────────────────────> S3 media bucket
Browser ── OAuth (code + PKCE) ────────────────────> Cognito managed login
```

Everything lives in **us-east-1** (CloudFront certificates must be there, so the whole stack is kept in one region).

## Repo layout

| Path | What |
| --- | --- |
| `api/` | Go module. One binary: runs as a plain HTTP server locally, or on Lambda behind API Gateway. |
| `web/` | Static frontend (vanilla HTML/CSS/JS for now; React + Vite port is phase 2). |
| `infra/bootstrap.yaml` | CloudFormation: the artifacts bucket that holds Lambda zips. |
| `infra/vidwatch.yaml` | CloudFormation: everything else. |
| `scripts/` | Build and deploy helpers. |

## Request flow

- **Pages**: a CloudFront Function maps pretty URLs to files: `/` → `/index.html`; `/login`, `/register`, `/upload`, `/watch`, `/trending`, `/library`, `/playlist`, `/settings` → `/<name>.html`; `/u/*` → `/profile.html`; `/auth/callback` → `/auth-callback.html`. Everything else passes through.
- **API**: `/api/*` goes to API Gateway (HTTP API, payload format 2.0, one `ANY /api/{proxy+}` route) and on to the Lambda. CloudFront adds an `X-Origin-Verify` header with a per-stack secret; the API rejects requests without it, so the `execute-api` URL can't be used directly. CloudFront forwards `CloudFront-Viewer-Address`, which the API uses as the client IP for anonymous view counting.
- **Media**: served from `https://media.vidwatch.burgett.dev/<key>`. CloudFront handles range requests (seeking) natively.
- **Caching**: site files have fixed names (`app.js`, `styles.css`), so CloudFront sends them with `Cache-Control: no-cache` and browsers revalidate on every load (a cheap 304). Each deploy invalidates CloudFront's own copy. Media keys are random and never reused, so media is cached normally.

## Auth (Cognito)

- User pool (Essentials tier) with managed login v2. Users sign up with **username + email + password** and can sign in with either username or email. Email is verified with a code. A pre-sign-up trigger enforces usernames of 3–24 letters, numbers, or underscores.
- Public app client, authorization code grant with PKCE, scopes `openid email profile`. Callback `https://<domain>/auth/callback` (plus `http://localhost:8080/auth/callback` for local dev). Access tokens last 1 hour, refresh tokens 30 days.
- The frontend sends the **access token** as `Authorization: Bearer <token>`. The API verifies it itself (RS256 against the pool's JWKS; checks `iss`, `exp`, `token_use == "access"`, `client_id`). Routes that work signed-out treat a missing or invalid token as anonymous; signed-in-only routes return 401.
- The user's ID everywhere is the Cognito `sub`. The display username is the Cognito `username` claim. The API creates the profile row the first time it sees a new user.

## Uploads

1. `POST /api/uploads` `{kind, contentType, size}` → `{key, url, fields, maxBytes}`.
   - `kind`: `video` (`video/mp4`, `video/webm`), or `thumbnail` / `avatar` / `banner` (`image/jpeg`, `image/png`, `image/webp`, `image/gif`).
   - Size limit: `MAX_UPLOAD_MB` for video, 10 MB for images.
   - Keys look like `videos/<userId>/<random>.mp4`.
2. The browser POSTs `multipart/form-data` straight to `url` with every entry of `fields`, then the file as the **last** field, named `file`. S3 enforces the exact key, Content-Type and size range. Success is HTTP 204.
3. The browser passes `key` to the next API call (`POST /api/videos`, `PATCH /api/videos/{id}`, `PATCH /api/users/me`). The API checks the key belongs to that user and that the object exists with an allowed type and size.

## API

All bodies are JSON. Errors are `{ "error": "message" }`. IDs are **strings** (videos: 11-character random IDs, so unlisted videos can't be guessed). Lists that page take `?offset=N` and return `hasMore`; page size is 24.

| Method & path | Auth | Notes |
| --- | --- | --- |
| `GET /api/config` | – | `{cognitoDomain, clientId, maxUploadMB}` |
| `GET /api/auth/me` | required | `{user}` |
| `GET /api/videos` | optional | `q, mine, following, rating, maxRating, duration, since, sort(new\|views\|likes), offset` → `{videos, hasMore}` |
| `GET /api/trending` | – | `maxRating, offset` → `{videos, hasMore}` |
| `GET /api/videos/{id}` | optional | `{video, liked, subscribed, savedForLater, related, reports, resumePosition}`; records a unique view |
| `GET /api/videos/{id}/download` | – | 302 to a short-lived S3 URL that downloads as `<title>.<ext>` |
| `POST /api/videos/{id}/progress` | required | `{position}` |
| `GET/DELETE /api/me/history`, `DELETE /api/me/history/{videoId}` | required | `{items:[{video, position, updatedAt}], hasMore}` |
| `GET/POST/DELETE /api/me/watch-later`, `DELETE /api/me/watch-later/{videoId}` | required | POST `{videoId}` → `{ok, saved, added}` |
| `GET /api/me/notifications`, `POST /api/me/notifications/seen` | required | `{items:[{video, unread}], unread}` |
| `POST /api/uploads` | required | see Uploads |
| `POST /api/videos` | required | `{title, description, ageRating, visibility, durationSeconds, videoKey, thumbnailKey?}` → `{video}` |
| `PATCH /api/videos/{id}` | owner | any of `{title, description, ageRating, visibility, thumbnailKey}` → `{video}` |
| `DELETE /api/videos/{id}` | owner | |
| `POST /api/videos/{id}/like` | required | toggles → `{liked, likes}` |
| `GET/POST /api/videos/{id}/comments` | GET optional | GET `?sort=new\|old` → `{comments}`; POST `{body, parentId?}` → `{comment}` |
| `PATCH/DELETE /api/videos/{id}/comments/{commentId}` | author (delete: author or video owner) | |
| `POST /api/videos/{id}/report` | required | `{reason}` → `{ok, alreadyReported}` |
| `POST /api/users/{username}/subscribe` | required | toggles → `{subscribed, subscribers}` |
| `GET /api/users/{username}` | optional | `{user, videos, hasMore}` |
| `GET /api/users/{username}/liked` | self | `{videos, hasMore}` |
| `PATCH /api/users/me` | required | any of `{bio, playbackSpeed, avatarKey, bannerKey, removeBanner}` → `{user}` |
| `GET/POST /api/playlists` | required | GET `?videoId=` adds `contains`; POST `{title}` |
| `GET/DELETE /api/playlists/{id}` | GET optional | |
| `POST /api/playlists/{id}/items`, `DELETE /api/playlists/{id}/items/{videoId}` | owner | POST `{videoId}` → `{ok, count}` |

Response shapes are the same as the original Node app, with string IDs and absolute media URLs:

- **video**: `{id, title, description, videoUrl, thumbnailUrl, views, likes, durationSeconds, ageRating, visibility, createdAt, author:{id, username, avatarUrl}}`
- **user**: `{id, username, bio, avatarUrl, bannerUrl, playbackSpeed}`
- **comment**: `{id, body, createdAt, editedAt, parentId, author}`
- **playlist summary**: `{id, title, createdAt, count, contains?}`

Times are Unix milliseconds.

## DynamoDB (single table)

Keys: `PK`, `SK`. Indexes, all projecting every attribute:
- `LSI1`: `PK` + `LSI1SK`
- `GSI1`: `GSI1PK` + `GSI1SK`
- `GSI2`: `GSI2PK` + `GSI2SK`

| Item | PK | SK | Index keys |
| --- | --- | --- | --- |
| User profile | `USER#<uid>` | `PROFILE` | |
| Username claim | `UNAME#<lowercase name>` | `UNAME` | |
| Video | `VIDEO#<vid>` | `META` | GSI1: `PUBLIC` / `<ts>#<vid>` (public only); GSI2: `CREATOR#<uid>` / `<ts>#<vid>` |
| Unique view | `VIDEO#<vid>` | `VIEW#u:<uid>` or `VIEW#ip:<ip>` | |
| Comment | `VIDEO#<vid>` | `COMMENT#<time-sortable id>` | |
| Report | `VIDEO#<vid>` | `REPORT#<uid>` | |
| Like | `USER#<uid>` | `LIKE#<vid>` | LSI1: `LIKE#<ts>`; GSI2: `REF#<vid>` |
| History | `USER#<uid>` | `HIST#<vid>` | LSI1: `HIST#<ts>`; GSI2: `REF#<vid>` |
| Watch later | `USER#<uid>` | `WL#<vid>` | LSI1: `WL#<ts>`; GSI2: `REF#<vid>` |
| Subscription | `USER#<follower>` | `SUB#<creator>` | |
| Playlist | `PLAYLIST#<pid>` | `META` | GSI2: `PLAYLISTS#<uid>` / `<ts>#<pid>` |
| Playlist item | `PLAYLIST#<pid>` | `ITEM#<vid>` | LSI1: `ITEM#<position>`; GSI2: `REF#<vid>` |

`<ts>` is a 13-digit zero-padded millisecond timestamp, so string order is time order.

- **Counters** (`views`, `likes` on videos; `subscribers` on users) change in the same transaction as the item that justifies them, so a like or unique view is counted exactly once.
- **Trending**: each video keeps a `viewDays` map (`YYYY-MM-DD` → count), trimmed to the last 8 days. Trending sums the last 7.
- **Deleting a video** removes its partition (views, comments, reports), every `REF#<vid>` item (likes, history, watch later, playlist entries), and its S3 objects.
- **Feed queries** (search, sort by views/likes, trending, following feed) read the public-video index and filter and sort in memory. That keeps substring search and every existing filter, and it's cheap up to a few thousand public videos. Past that, add a real search index.
