#!/usr/bin/env bash
# Build and deploy everything from the command line (the console steps in the
# README do the same thing by hand).
#
#   HOSTED_ZONE_ID=Z0123456789ABC CERTIFICATE_ARN=arn:aws:acm:us-east-1:… scripts/deploy.sh   # first deploy
#   scripts/deploy.sh                                        # later deploys
#   scripts/deploy.sh MaxUploadMB=1000                       # extra parameter overrides
#
# Environment:
#   STACK_NAME       main stack name           (default: vidwatch)
#   BOOTSTRAP_STACK  artifacts bucket stack    (default: vidwatch-bootstrap)
#   HOSTED_ZONE_ID   Route 53 zone for the domains, and
#   CERTIFICATE_ARN  the existing us-east-1 ACM certificate covering both domains;
#                    both required the first time, later deploys keep the
#                    previous values when they're unset.
#
# Needs AWS CLI v2 with credentials for the target account, Go, and zip.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STACK_NAME="${STACK_NAME:-vidwatch}"
BOOTSTRAP_STACK="${BOOTSTRAP_STACK:-vidwatch-bootstrap}"
HOSTED_ZONE_ID="${HOSTED_ZONE_ID:-}"
CERTIFICATE_ARN="${CERTIFICATE_ARN:-}"

# Everything lives in us-east-1 (CloudFront certificates must be there).
export AWS_REGION=us-east-1
export AWS_DEFAULT_REGION=us-east-1

die() { echo "error: $*" >&2; exit 1; }

command -v aws >/dev/null 2>&1 || die "the AWS CLI v2 is required (https://aws.amazon.com/cli/)."
aws --version 2>&1 | grep -q '^aws-cli/2\.' || die "AWS CLI v2 is required; found: $(aws --version 2>&1)"

stack_output() { # stack, output key
  aws cloudformation describe-stacks --stack-name "$1" \
    --query "Stacks[0].Outputs[?OutputKey=='$2'].OutputValue" --output text
}

echo "==> Bootstrap stack ($BOOTSTRAP_STACK)"
aws cloudformation deploy \
  --stack-name "$BOOTSTRAP_STACK" \
  --template-file "$ROOT/infra/bootstrap.yaml" \
  --no-fail-on-empty-changeset
ARTIFACTS_BUCKET="$(stack_output "$BOOTSTRAP_STACK" ArtifactsBucketName)"
[ -n "$ARTIFACTS_BUCKET" ] && [ "$ARTIFACTS_BUCKET" != "None" ] || die "couldn't read ArtifactsBucketName from $BOOTSTRAP_STACK"

echo "==> Building the API"
ZIP_PATH="$("$ROOT/scripts/build-api.sh")"
CODE_KEY="api/$(basename "$ZIP_PATH")"
if aws s3api head-object --bucket "$ARTIFACTS_BUCKET" --key "$CODE_KEY" >/dev/null 2>&1; then
  echo "    s3://$ARTIFACTS_BUCKET/$CODE_KEY already uploaded"
else
  aws s3 cp "$ZIP_PATH" "s3://$ARTIFACTS_BUCKET/$CODE_KEY"
fi

PARAMS=("ArtifactsBucket=$ARTIFACTS_BUCKET" "ApiCodeKey=$CODE_KEY")
[ -n "$HOSTED_ZONE_ID" ] && PARAMS+=("HostedZoneId=$HOSTED_ZONE_ID")
[ -n "$CERTIFICATE_ARN" ] && PARAMS+=("CertificateArn=$CERTIFICATE_ARN")
if ! aws cloudformation describe-stacks --stack-name "$STACK_NAME" >/dev/null 2>&1; then
  [ -n "$HOSTED_ZONE_ID" ] || die "HOSTED_ZONE_ID is required for the first deploy (the Route 53 zone ID for burgett.dev)."
  [ -n "$CERTIFICATE_ARN" ] || die "CERTIFICATE_ARN is required for the first deploy (the us-east-1 ACM certificate for vidwatch.burgett.dev + *.vidwatch.burgett.dev)."
fi
PARAMS+=("$@")

echo "==> Main stack ($STACK_NAME) — a first deploy takes several minutes (CloudFront)"
# Parameters not listed keep their previous values on updates.
aws cloudformation deploy \
  --stack-name "$STACK_NAME" \
  --template-file "$ROOT/infra/vidwatch.yaml" \
  --capabilities CAPABILITY_IAM \
  --no-fail-on-empty-changeset \
  --parameter-overrides "${PARAMS[@]}"

SITE_BUCKET="$(stack_output "$STACK_NAME" SiteBucketName)"
DISTRIBUTION_ID="$(stack_output "$STACK_NAME" SiteDistributionId)"
SITE_URL="$(stack_output "$STACK_NAME" SiteUrl)"

echo "==> Uploading web/ to s3://$SITE_BUCKET"
# CloudFront adds Cache-Control: no-cache to site files (see
# SiteResponseHeadersPolicy), so a plain sync is enough.
aws s3 sync "$ROOT/web/" "s3://$SITE_BUCKET/" --delete

echo "==> Invalidating CloudFront ($DISTRIBUTION_ID)"
aws cloudfront create-invalidation --distribution-id "$DISTRIBUTION_ID" --paths '/*' \
  --query 'Invalidation.Id' --output text >/dev/null

echo "Done: $SITE_URL"
