#!/usr/bin/env bash
# Build the Go API for Lambda (provided.al2023, arm64) and zip it.
#
#   scripts/build-api.sh
#
# The zip is named after a hash of the binary, so unchanged code produces the
# same file and the same S3 key. Prints the zip's path on stdout (so other
# scripts can capture it); progress and instructions go to stderr.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
API="$ROOT/api"

command -v go >/dev/null 2>&1 || { echo "error: 'go' is not on PATH. Install Go (see api/go.mod for the version) and try again." >&2; exit 1; }
command -v zip >/dev/null 2>&1 || { echo "error: 'zip' is not installed." >&2; exit 1; }

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

cd "$API"
mkdir -p dist
rm -f dist/bootstrap

echo "Building api (linux/arm64)..." >&2
# -buildvcs=false keeps git state out of the binary, so the hash only changes
# when the code does.
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -tags lambda.norpc -trimpath -buildvcs=false -ldflags="-s -w" \
  -o dist/bootstrap ./cmd/vidwatch-api

hash="$(sha256 dist/bootstrap | cut -c1-12)"
zipname="vidwatch-api-${hash}.zip"
rm -f "dist/${zipname}"
(cd dist && zip -q -X "${zipname}" bootstrap)

{
  echo "Built:  api/dist/${zipname}"
  echo "Upload: s3://<ArtifactsBucket>/api/${zipname}"
  echo "Then set the stack parameter ApiCodeKey=api/${zipname}"
} >&2

echo "${API}/dist/${zipname}"
