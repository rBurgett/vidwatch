# vidwatch

A YouTube-like video sharing app: sign up, upload videos, browse and search, watch with seeking and resume, like, comment, follow creators, build playlists, and more.

It runs at **vidwatch.burgett.dev** on AWS:

- **Frontend**: static files on S3 behind CloudFront.
- **API**: Go on Lambda behind API Gateway.
- **Data**: DynamoDB.
- **Sign-in**: Cognito.
- **Media**: uploads go straight from the browser to S3 and are served from `media.vidwatch.burgett.dev`.

The original Node/Express/SQLite version is kept on the `legacy` branch. [docs/architecture.md](docs/architecture.md) covers the design: request flow, API, DynamoDB key layout, and upload flow.

## Layout

```
api/                 Go API (one binary: Lambda in AWS, plain HTTP server locally)
  cmd/vidwatch-api/  entry point
  cmd/devsetup/      creates the table + bucket in local emulators
  internal/          server (HTTP), store (DynamoDB), media (S3), auth (Cognito JWTs)
web/                 static frontend (vanilla HTML/CSS/JS, no build step)
infra/bootstrap.yaml CloudFormation: bucket for build artifacts
infra/vidwatch.yaml  CloudFormation: everything else
scripts/             build-api.sh, deploy.sh
```

## Deploying

You need Go 1.25+ and `zip`. The AWS CLI v2 is only needed for `scripts/deploy.sh`.

Everything goes in **us-east-1**: CloudFront requires its certificates there, and the main template refuses to deploy anywhere else. The stack uses your existing ACM certificate for `vidwatch.burgett.dev` and `*.vidwatch.burgett.dev` (which covers `media.vidwatch.burgett.dev`); it must also be in us-east-1.

### With the script

```bash
HOSTED_ZONE_ID=<burgett.dev zone id> \
CERTIFICATE_ARN=<arn of the vidwatch.burgett.dev certificate> \
  scripts/deploy.sh          # first time
scripts/deploy.sh            # after that
```

It runs these steps in order:
1. Creates or updates the bootstrap stack.
2. Builds the API and uploads the zip.
3. Creates or updates the `vidwatch` stack.
4. Syncs `web/` to the site bucket.
5. Invalidates CloudFront.

### With the console

1. **Bootstrap stack.** CloudFormation → Create stack → upload `infra/bootstrap.yaml`, name it `vidwatch-bootstrap`. Note the `ArtifactsBucketName` output.
2. **API zip.** Run `scripts/build-api.sh`. Upload the zip it prints to the artifacts bucket under `api/`, so the key is `api/vidwatch-api-<hash>.zip`.
3. **Main stack.** CloudFormation → Create stack → upload `infra/vidwatch.yaml`, name it `vidwatch`.
   - Set **CertificateArn** (your existing ACM certificate for `vidwatch.burgett.dev` + `*.vidwatch.burgett.dev`), **HostedZoneId** (pick burgett.dev), **ArtifactsBucket** (from step 1) and **ApiCodeKey** (`api/vidwatch-api-<hash>.zip`).
   - Leave the rest at their defaults, acknowledge IAM, and create.
   - The first create takes several minutes, mostly CloudFront.
4. **Site files.** Upload the *contents* of `web/` to the root of the `SiteBucketName` output.

**To update:**
- **API:** build, upload the new zip, then Update stack → use the current template → change `ApiCodeKey`.
- **Site:** re-upload `web/`, then create a CloudFront invalidation for `/*` on `SiteDistributionId`. CloudFront tells browsers to revalidate site files, so they pick up changes right away.

### Before inviting people

- **Email:** Cognito's built-in email sender is limited to a few dozen messages a day (sign-up codes, password resets). Move it to SES before launch: set `EmailConfiguration` to `DEVELOPER` with a verified SES identity.
- **Settings you can't change later:** the Cognito domain prefix (`vidwatch-burgett`) must be unused across AWS. The user pool's sign-in settings (username + email alias, case-insensitive) can't be changed after creation, so check them before the first deploy.
- **Retained resources:** the DynamoDB table, media bucket and user pool use `DeletionPolicy: Retain` plus deletion protection. Deleting the stack leaves them behind on purpose, so remove them by hand if you ever tear everything down.
- **Default sign-in page:** the managed login page uses Cognito's default look. Customize it in the Cognito console's branding editor.

## Local development

The API runs as a normal HTTP server and serves `web/` too.

- **Local data:** DynamoDB Local and LocalStack (for S3), in containers.
- **Sign-in:** goes through the real Cognito pool, which allows `http://localhost:8080/auth/callback`. So deploy the stack once first.

```bash
podman run -d --name vidwatch-ddb -p 8000:8000 amazon/dynamodb-local -jar DynamoDBLocal.jar -sharedDb
podman run -d --name vidwatch-s3 -p 4566:4566 -e SERVICES=s3 localstack/localstack:4.4

cd api
cp .env.example .env          # then paste the Cognito IDs from the stack outputs
go run ./cmd/devsetup         # create the table + bucket (only ever touches the emulators)
go run ./cmd/vidwatch-api     # http://localhost:8080
```

(`docker` works the same as `podman`.)

## Tests

```bash
cd api
go test ./...                 # unit tests

# Full API + browser tests against the emulators (browser tests need Chromium/Chrome on PATH):
VIDWATCH_IT_DYNAMODB=http://127.0.0.1:8000 VIDWATCH_IT_S3=http://127.0.0.1:4566 \
  go test -tags integration ./...
```

The integration suite runs real requests through the API, DynamoDB Local and S3:
- uploads, including S3 rejecting a file whose type or size doesn't match what was signed
- views, likes, comments, notifications, playlists, unlisted videos
- cleanup when a video is deleted

It also loads the main pages in headless Chromium and walks the whole sign-in flow against a fake Cognito.

## Known limits / next steps

- **Feeds:** search, sort by views/likes, trending and the "following" feed read every public video and filter in memory. That's fine up to a few thousand public videos; past that, add a search index.
- **Video formats:** only MP4 and WebM are accepted. Converting other formats (MediaConvert) is a later step.
- **Abandoned uploads:** an upload that finishes but is never attached to a video (e.g. the tab closed mid-publish) stays in S3. Add a lifecycle rule or sweeper if it adds up.
- **Phase 2:** port `web/` to React + TypeScript + Vite.
- **Still not built:** moderation/admin tools, captions, adaptive streaming (HLS), account deletion.
