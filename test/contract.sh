#!/usr/bin/env bash
set -euo pipefail
BASE=http://localhost:8099
KEY=testkey123
APP=com.donetick.app
fail() { echo "FAIL: $*"; exit 1; }
check() { echo "$2" | grep -q "$1" || fail "expected '$1' in: $2"; }

# --- build a fake dist bundle ---
W=$(mktemp -d)
mkdir -p "$W/dist/assets"
echo '<html><body>v1</body></html>' > "$W/dist/index.html"
echo 'console.log(1)' > "$W/dist/assets/app.js"
(cd "$W/dist" && zip -qr ../good.zip .)
(cd "$W" && zip -qr bad.zip dist)   # the classic wrong-shape zip

echo "--- 1. upload with wrong zip shape is rejected"
R=$(curl -s -H "Authorization: Bearer $KEY" -F version=9.9.9 -F file=@"$W/bad.zip" "$BASE/api/apps/$APP/bundles")
check "invalid_bundle" "$R"; echo "ok: $R"

echo "--- 2. unauthenticated upload is rejected"
R=$(curl -s -o /dev/null -w '%{http_code}' -F version=1.0.0 -F file=@"$W/good.zip" "$BASE/api/apps/$APP/bundles")
[ "$R" = 401 ] || fail "expected 401, got $R"; echo "ok: 401"

echo "--- 3. upload a real bundle"
R=$(curl -s -H "Authorization: Bearer $KEY" -F version=1.2.48 -F file=@"$W/good.zip" "$BASE/api/apps/$APP/bundles")
check '"version":"1.2.48"' "$R"
SERVER_SUM=$(echo "$R" | sed 's/.*"checksum":"\([a-f0-9]*\)".*/\1/')
LOCAL_SUM=$(shasum -a 256 "$W/good.zip" | cut -d' ' -f1)
[ "$SERVER_SUM" = "$LOCAL_SUM" ] || fail "checksum mismatch: $SERVER_SUM vs $LOCAL_SUM"
echo "ok: sha256 $SERVER_SUM matches local"

echo "--- 4. duplicate version is rejected"
R=$(curl -s -H "Authorization: Bearer $KEY" -F version=1.2.48 -F file=@"$W/good.zip" "$BASE/api/apps/$APP/bundles")
check "version_exists" "$R"; echo "ok"

infos() { cat <<EOF
{"platform":"android","device_id":"$1","app_id":"$APP","custom_id":"",
 "version_build":"1.2.47","version_code":"12047","version_os":"14",
 "version_name":"$2","plugin_version":"8.51.13","is_emulator":false,"is_prod":true,
 "install_source":"play","defaultChannel":"${3:-}"${4:-}}
EOF
}

echo "--- 5. update check before release: no update"
R=$(curl -s -X POST -H 'Content-Type: application/json' -d "$(infos dev-a builtin)" "$BASE/updates")
check "No new version available" "$R"; echo "ok: $R"

echo "--- 6. release to production channel"
R=$(curl -s -X POST -H "Authorization: Bearer $KEY" "$BASE/api/apps/$APP/channels/production/bundle?version=1.2.48")
check '"status":"ok"' "$R"; echo "ok"

echo "--- 7. update check now returns the bundle"
R=$(curl -s -X POST -H 'Content-Type: application/json' -d "$(infos dev-a builtin)" "$BASE/updates")
check '"version":"1.2.48"' "$R"; check '"checksum":"'"$LOCAL_SUM" "$R"; check "$BASE/bundles/$APP/1.2.48.zip" "$R"
echo "ok: $R"

echo "--- 8. device already on that version gets no update"
R=$(curl -s -X POST -H 'Content-Type: application/json' -d "$(infos dev-a 1.2.48)" "$BASE/updates")
check "No new version available" "$R"; echo "ok"

echo "--- 9. bundle downloads and matches the advertised checksum"
curl -sf "$BASE/bundles/$APP/1.2.48.zip" -o "$W/dl.zip" || fail "download failed"
DL_SUM=$(shasum -a 256 "$W/dl.zip" | cut -d' ' -f1)
[ "$DL_SUM" = "$LOCAL_SUM" ] || fail "downloaded checksum mismatch"
unzip -l "$W/dl.zip" > "$W/listing.txt"
grep -q " index.html" "$W/listing.txt" || fail "index.html not at zip root"
echo "ok: downloaded, checksum matches, index.html at root"

echo "--- 10. path traversal is blocked"
R=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/bundles/$APP/..%2f..%2fcapgo.db")
[ "$R" = 404 ] || fail "expected 404, got $R"; echo "ok: 404"

echo "--- 11. beta channel: not self-assignable by default"
curl -s -X POST -H "Authorization: Bearer $KEY" "$BASE/api/apps/$APP/channels/beta" > /dev/null
R=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d "$(infos dev-b builtin '' ',"channel":"beta"')" "$BASE/channel_self")
check "channel_not_self_assignable" "$R"; echo "ok: $R"

echo "--- 12. make beta self-assignable, then a device pins itself to it"
curl -s -X POST -H "Authorization: Bearer $KEY" "$BASE/api/apps/$APP/channels/beta?allow_self_set=true" > /dev/null
R=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d "$(infos dev-b builtin '' ',"channel":"beta"')" "$BASE/channel_self")
check '"status":"ok"' "$R"; echo "ok"

echo "--- 13. PUT reports the pinned channel"
R=$(curl -s -X PUT -H 'Content-Type: application/json' -d "$(infos dev-b builtin)" "$BASE/channel_self")
check '"channel":"beta"' "$R"; echo "ok: $R"

echo "--- 14. beta device gets no update (beta channel is empty), prod device still does"
R=$(curl -s -X POST -H 'Content-Type: application/json' -d "$(infos dev-b builtin)" "$BASE/updates")
check "No new version available" "$R"
R=$(curl -s -X POST -H 'Content-Type: application/json' -d "$(infos dev-c builtin)" "$BASE/updates")
check '"version":"1.2.48"' "$R"
echo "ok: channels isolate correctly"

echo "--- 15. GET lists only channels the device may pick"
R=$(curl -s "$BASE/channel_self?app_id=$APP&device_id=dev-b")
check '"name":"beta"' "$R"; check '"name":"production"' "$R"; echo "ok: $R"

echo "--- 16. DELETE unpins the device, it falls back to production"
curl -s -X DELETE -H 'Content-Type: application/json' -d "$(infos dev-b builtin)" "$BASE/channel_self" > /dev/null
R=$(curl -s -X POST -H 'Content-Type: application/json' -d "$(infos dev-b builtin)" "$BASE/updates")
check '"version":"1.2.48"' "$R"; echo "ok"

echo "--- 17. stats are accepted and recorded"
R=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d "$(infos dev-a 1.2.48 '' ',"action":"update","old_version_name":"builtin"')" "$BASE/stats")
check '"status":"ok"' "$R"; echo "ok"

echo "--- 18. min_native gate blocks an old native shell"
(cd "$W/dist" && echo v2 > index.html && zip -qr ../good2.zip .)
curl -s -H "Authorization: Bearer $KEY" -F version=1.3.0 -F min_native=2.0.0 \
  -F file=@"$W/good2.zip" -F channel=production "$BASE/api/apps/$APP/bundles" > /dev/null
R=$(curl -s -X POST -H 'Content-Type: application/json' -d "$(infos dev-a 1.2.48)" "$BASE/updates")
check "older than required 2.0.0" "$R"; echo "ok: $R"

echo "--- 19. admin UI requires auth and renders"
R=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/admin")
[ "$R" = 401 ] || fail "expected 401 on /admin, got $R"
R=$(curl -s -u "admin:$KEY" "$BASE/admin")
check "1.2.48" "$R"; check "capgo-selfhost" "$R"; echo "ok: admin renders bundles"

echo "--- 20. defaultChannel from the app config reaches a private channel"
# Regression: defaultChannel is compiled into the binary, so it must work for a
# channel that is neither public nor self-assignable. Gating it on those flags
# made staging builds silently fall back to production.
curl -s -X POST -H "Authorization: Bearer $KEY" "$BASE/api/apps/$APP/channels/staging" > /dev/null
curl -s -X POST -H "Authorization: Bearer $KEY" "$BASE/api/apps/$APP/channels/staging/bundle?version=1.2.48" > /dev/null
R=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d "$(infos dev-d builtin staging)" "$BASE/updates")
check '"version":"1.2.48"' "$R"; echo "ok: $R"

echo "--- 21. ...but runtime self-assignment to it is still refused"
R=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d "$(infos dev-d builtin '' ',"channel":"staging"')" "$BASE/channel_self")
check "channel_not_self_assignable" "$R"; echo "ok"

rm -rf "$W"
echo
echo "ALL 21 CHECKS PASSED"
