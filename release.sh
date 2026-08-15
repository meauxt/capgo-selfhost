#!/usr/bin/env bash
# Zip a built web directory and release it to a channel.
#
#   CAPGO_URL=https://updates.example.com CAPGO_KEY=... \
#     ./release.sh com.example.app 1.2.48 ./dist production
#
set -euo pipefail

APP=${1:?usage: release.sh <app_id> <version> <dist_dir> [channel]}
VERSION=${2:?missing version}
DIST=${3:?missing dist dir}
CHANNEL=${4:-production}
: "${CAPGO_URL:?set CAPGO_URL}"
: "${CAPGO_KEY:?set CAPGO_KEY}"

[ -f "$DIST/index.html" ] || {
  echo "error: $DIST/index.html not found — point this at the built web output" >&2
  exit 1
}

ZIP=$(mktemp -t capgo-bundle).zip
trap 'rm -f "$ZIP"' EXIT
# Zip the *contents* of dist. A zip of the folder itself gives every file a
# "dist/" prefix and the plugin finds no index.html at the root.
(cd "$DIST" && zip -qr "$ZIP" . -x '.DS_Store' '*/.DS_Store')

echo "uploading $APP $VERSION ($(du -h "$ZIP" | cut -f1)) to $CHANNEL"
RESPONSE=$(curl -sS -w '\n%{http_code}' -H "Authorization: Bearer $CAPGO_KEY" \
  -F "version=$VERSION" -F "channel=$CHANNEL" -F "file=@$ZIP" \
  "$CAPGO_URL/api/apps/$APP/bundles")

BODY=$(echo "$RESPONSE" | sed '$d')
CODE=$(echo "$RESPONSE" | tail -1)
echo "$BODY"
[ "$CODE" = 200 ] || { echo "upload failed (HTTP $CODE)" >&2; exit 1; }

echo "released: devices on '$CHANNEL' will pick up $VERSION on next app open"
