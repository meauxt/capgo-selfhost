# capgo-selfhost

A small self-hosted update server for
[`@capgo/capacitor-updater`](https://github.com/Cap-go/capacitor-updater).

The plugin is open source and its self-hosted mode just needs an HTTP endpoint
that answers "is there a newer bundle?". This is that endpoint — one Go binary,
one SQLite file, one directory of zips. It is **not** a rebuild of Capgo Cloud:
no analytics, no org/team management, no encryption or delta updates.

Ships in a 35 MB image, idles at ~5 MB RSS, answers an update check in ~6 ms.

---

## Why this exists

Three options were evaluated for self-hosting OTA updates on a small ARM box
(2 vCPU, 10 GB RAM, ~8 GB free disk, already running Coolify).

**1. The real Capgo console (`Cap-go/capgo.app`, AGPL-3.0).** Rejected. It
needs self-hosted Supabase (~10 containers), a set of Deno edge functions, S3
storage and a Vue frontend — and the upstream repo contains **no Dockerfile and
no compose file**, so there is no supported container path. It would have been a
hand-built integration dominating the host, with a manual re-integration at
every upstream release. Right answer if you want the whole product; wrong answer
if you want updates to reach phones.

**2. `tanapoln/capgo-server` (third-party, MIT).** Rejected on a hard fact: the
published image is **amd64-only**, and the target host is aarch64. It also
brings MongoDB and an S3 dependency for what is fundamentally a key-value
lookup.

**3. Implement the plugin's self-hosted contract directly.** Chosen. The
contract turns out to be small — a handful of JSON endpoints and a zip download
— and implementing it exactly removes every moving part that isn't strictly
required.

The deciding insight: **the plugin already does the hard parts.** Download,
checksum verification, atomic bundle swap, failed-boot rollback, and the
delay/retry logic all live in native code on the device. The server's entire job
is to answer one question correctly and hand over a file.

---

## How it works

```mermaid
graph LR
  subgraph dev[Your machine / CI]
    B[npm run build] --> Z[zip of dist/]
  end

  subgraph srv[capgo-selfhost]
    API[POST /api/apps/.../bundles]
    DB[(SQLite<br/>bundles · channels<br/>devices · stats)]
    FS[/data/bundles/*.zip]
    UPD[POST /updates]
    DL[GET /bundles/...zip]
  end

  subgraph dev2[Phones]
    P[capacitor-updater<br/>on app open]
  end

  Z -->|upload + set channel| API
  API --> DB
  API --> FS
  P -->|AppInfos| UPD
  UPD --> DB
  UPD -->|version, url, checksum| P
  P -->|download| DL
  DL --> FS
```

On every app open the plugin POSTs a description of the device to `/updates`.
The server resolves which channel that device belongs to, looks up the bundle
the channel points at, and either returns `{version, url, checksum}` or
`{message: "No new version available"}`. The plugin downloads the zip, verifies
the SHA-256, unpacks it, and swaps it in on the next launch.

### The contract

Request and response shapes were taken from **the plugin's Android source**
(`CapgoUpdater.createInfoObject`, `CapacitorUpdaterPlugin`, `CryptoCipher`), not
from the published docs. That was a deliberate methodology choice and it
mattered: the docs omit `sessionKey`, omit three of the four `/channel_self`
verbs, omit the stats payload, and do not say that `version_code` arrives as a
string. Building against the docs alone would have produced a server that looks
correct and fails on device.

| Endpoint | Method | Purpose |
|---|---|---|
| `/updates` | POST | Update check → `{version, url, checksum}` or `{message}` |
| `/stats` | POST | Plugin events (`update`, `download_fail`, `checksum_fail`, …) |
| `/channel_self` | GET | Lists channels the device may self-assign to (bare JSON array) |
| `/channel_self` | PUT | Reports the device's current channel |
| `/channel_self` | POST | Self-assigns the device to a channel |
| `/channel_self` | DELETE | Clears the assignment |
| `/bundles/{app}/{version}.zip` | GET | Serves the bundle, unauthenticated |
| `/healthz` | GET | Liveness |
| `/admin` | GET | Web UI, HTTP basic auth |
| `/api/…` | — | Management API, `Authorization: Bearer $API_KEY` |

Not implemented: bundle encryption (`sessionKey`), delta/manifest updates,
organisations. Devices fall back to full-zip downloads, which is the plugin's
default anyway.

---

## Design decisions and trade-offs

Each of these is a real fork in the road, with what it costs.

### Go, single static binary, no CGO

**Why.** The target is a shared 2-vCPU ARM box. A Go binary with
`CGO_ENABLED=0` cross-compiles trivially to `linux/arm64`, produces a 35 MB
image on `alpine`, and idles at ~5 MB RSS — small enough to be invisible next to
Coolify and the other resources on the host.

**Cost.** No ecosystem of ready-made admin frameworks; the UI is hand-written
HTML. A Node or Bun service would have had a shorter path to a rich console.

### SQLite (pure-Go `modernc.org/sqlite`), not Postgres

**Why.** The entire dataset is a few hundred rows and some file metadata.
Postgres would have been a second container, a second backup target and a second
thing to patch, for data that fits in a file. `modernc.org/sqlite` is a pure-Go
SQLite, which is what keeps `CGO_ENABLED=0` — `mattn/go-sqlite3` would have
forced a C toolchain and a fatter, arch-specific build.

**Cost.** Single-writer. The pool is deliberately capped at
`SetMaxOpenConns(1)` because SQLite serializes writes anyway and a larger pool
just converts contention into `database is locked` errors. **Every update check
performs a write** (`UpsertDevice`), so throughput is bounded by SQLite write
throughput rather than by read speed — see *Known limits* below. No replication,
no HA, no horizontal scaling: this is a single-node design on purpose.

### Bundles on the local filesystem, not S3

**Why.** Bundles are a couple of MB and are written once, read many. A local
directory means no credentials, no egress bill, no third party in the update
path, and a backup story that is `cp -r`.

**Cost.** Ties the service to one node and one disk. No CDN, so every download
comes off the origin. Migrating to S3 later means presigned URLs and a change to
`bundleURL()` — contained, but not free. If bundles get large or traffic gets
global, this is the first thing to revisit.

### Server-rendered admin UI with HTTP basic auth

**Why.** An SPA would have needed a build step, a token flow, session handling
and a second thing to keep in sync with the API. The admin surface is four
tables and five forms; server-rendered HTML with `html/template` does that in
one file with no JavaScript and no auth state to get wrong.

**Cost.** No logout, no password rotation without a redeploy, no per-user
accounts, no audit trail of who released what. Basic-auth credentials live in
the browser's password store. Fine for one or two operators; wrong for a team.

### Bundle downloads are unauthenticated

**Why.** This matches Capgo Cloud, and more importantly matches the plugin: it
fetches the URL with no credentials. Any auth scheme would have to be encoded in
the URL, which is not meaningfully different from an unguessable path.

**Cost.** Anyone who learns a bundle URL can download your web assets. Those
assets already ship inside the installed app, so this leaks nothing new — but it
is not secrecy, and it should not be treated as such. If bundle contents ever
need to be confidential, the answer is the plugin's encryption support
(`sessionKey`), which this server does not implement.

### "Different version" means update — not "greater version"

**Why.** The server serves whatever the channel points at, and only stays quiet
when the device already runs that exact version. This makes rollback a
first-class operation: point the channel at an older bundle and devices come
back to it on next open. A semver-greater-than rule would have made rollback
impossible without inventing version numbers.

**Cost.** No protection against an accidental downgrade — if you point a channel
at the wrong bundle, devices obey. The channel pointer is the source of truth,
so it is also the thing to be careful with.

### `defaultChannel` is honoured for private channels

Initially `defaultChannel` was gated on `public || allow_self_set`. That was
wrong, and end-to-end testing caught it: a staging build asking for the staging
channel fell silently back to production.

**Why the current behaviour.** `defaultChannel` is compiled into the app binary
— it is a build-time decision by whoever produced the build, not a runtime
request from an arbitrary device. `allow_self_set` gates only the runtime
`setChannel()` path, where the request genuinely does originate on the device.

**Cost.** A crafted request can name any existing channel and receive its
bundle. The threat model is thin — the payload is your own JS and channel names
are not secrets — but it is a real difference from a strict-allowlist design.

### Versions are immutable

Re-uploading an existing version returns `409` instead of replacing the file.

**Why.** Devices cache by version. Silently swapping the bytes behind a version
that some devices have already downloaded produces a fleet running two different
builds under one name — an extremely unpleasant thing to debug.

**Cost.** You must bump the version for every release, including a one-character
fix. `npm version patch` makes this a non-issue in practice.

### Uploads are validated, not trusted

The server opens the zip and rejects it if there is no `index.html` at the root,
naming the offending top-level entry in the error.

**Why.** Zipping the `dist` folder instead of its contents is *the* recurring
OTA mistake. Undetected, it produces a bundle that uploads cleanly, installs
cleanly, and shows a blank screen on every device — with a rollback that hides
the evidence. Catching it at upload time turns a fleet-wide incident into an
error message.

**Cost.** A few hundred milliseconds per upload, and an assumption that every
bundle is a web root. Non-web payloads are not supported, which is fine — the
plugin does not support them either.

Uploads are written to a temp file and `rename`d into place, so a failed or
interrupted upload can never leave a partial zip where a device could fetch it.

### Defensive parsing of the device payload

`version_code` and friends arrive as strings on Android but have historically
differed by platform, so they are decoded through a `flexString` type that
accepts both JSON strings and numbers. A type mismatch in one field would
otherwise fail the entire update check.

`/stats` never returns an error to the plugin: stats are best-effort telemetry
and must not cause a retry loop or interfere with updating.

### Bounded growth

`stats` is pruned to the most recent 50 000 rows once a day. On a box with a few
GB of free disk, an unbounded event table is a slow-motion outage.

### One container, no queue, no worker

**Why.** Every operation is either a sub-millisecond database lookup or a file
write. There is no background work to schedule.

**Cost.** Uploads are synchronous, so a large bundle over a slow link holds a
request open — hence the 5-minute read/write timeouts.

---

## Configuration

| Env | Required | Default | Notes |
|---|---|---|---|
| `API_KEY` | yes | — | Protects uploads and (by default) the admin UI |
| `PUBLIC_URL` | yes | `http://localhost:8080` | **Must be HTTPS in production** |
| `DATA_DIR` | no | `/data` | SQLite file + bundle zips |
| `PORT` | no | `8080` | |
| `ADMIN_USER` | no | `admin` | |
| `ADMIN_PASSWORD` | no | value of `API_KEY` | |

`PUBLIC_URL` matters more than it looks: iOS and Android both refuse to
download a bundle over plain HTTP, and the plugin swallows the failure — the
server looks healthy in `curl` and no device ever updates. The server logs a
warning at boot if it sees a non-HTTPS public URL.

Mount a persistent volume at `DATA_DIR`. Without one, every bundle and the whole
database vanish on redeploy.

---

## App configuration

```ts
// capacitor.config.ts
plugins: {
  CapacitorUpdater: {
    updateUrl:  'https://updates.example.com/updates',
    statsUrl:   'https://updates.example.com/stats',
    channelUrl: 'https://updates.example.com/channel_self',
    autoUpdate: true,
  },
}
```

Your app **must** call `CapacitorUpdater.notifyAppReady()` once the web layer
has booted. If it does not, the plugin assumes the new bundle crashed and rolls
back to the previous one on the next launch — an update that appears to install
and then silently reverts is almost always this.

The updater is a native plugin, so **OTA only reaches devices running a store
build that already contains it**. The first release after adding it must go
through the store; everyone who installs that build is reachable from then on.

### Service workers

If your web app registers a service worker, guard it with
`!Capacitor.isNativePlatform()`. A Workbox precache from the previous bundle can
keep serving stale assets after the plugin swaps bundles, producing an update
that installs and visibly does nothing.

---

## Releasing

```bash
npm run build
CAPGO_URL=https://updates.example.com CAPGO_KEY=… \
  ./release.sh com.example.app 1.2.48 ./dist production
```

Or upload through `/admin` and pick a channel from the dropdown.

Rollback is pointing the channel back at an older version:

```bash
curl -X POST -H "Authorization: Bearer $CAPGO_KEY" \
  "$CAPGO_URL/api/apps/com.example.app/channels/production/bundle?version=1.2.47"
```

---

## Channels

Every app gets a `production` channel on first contact, marked public — that is
the fallback for any device with no explicit assignment. Additional channels
(`beta`, `staging`) are created from the admin UI or the API.

- **public**: the default channel for unassigned devices. Exactly one per app;
  marking a channel public clears the flag on the others, because otherwise
  fallback resolution depends on row ordering.
- **allow_self_set**: the app may move itself onto the channel at runtime via
  `CapacitorUpdater.setChannel()`. Leave it off for channels you assign
  yourself.

Resolution order for an update check is: the channel the device was explicitly
assigned to → the `defaultChannel` the app declares in its Capacitor config →
the app's public channel.

---

## Bundle format

A zip of the **contents** of your build output, with `index.html` at the root.
Zipping the `dist` folder itself is the most common mistake, so uploads that
have no root `index.html` are rejected with an explanatory error rather than
being accepted and failing on the device.

## `min_native`

An OTA update ships web assets only — it cannot add native code. If a bundle
needs a plugin that only exists in a newer app-store build, set `min_native` to
that native version and older shells will be told to stay put instead of
downloading a bundle that would crash on them.

---

## Operational characteristics

Measured on an Ampere ARM host (2 vCPU, 10 GB RAM):

| | |
|---|---|
| Image size | 35.5 MB |
| Idle memory | ~5 MB RSS |
| Update check | ~6 ms |
| Containers | 1 |
| External dependencies | none |

## Known limits, and when to outgrow this

- **Write-per-check.** Each update check upserts a device row. With SQLite in
  WAL mode this comfortably handles hundreds of checks per second, which is a
  large fleet given each device checks in only on app open. If it ever became
  the bottleneck, the fix is to make the device upsert asynchronous or sampled,
  not to change database.
- **Single node.** No HA. If the box is down, devices simply do not update; they
  keep running the bundle they already have, so an outage here is not
  user-visible.
- **No CDN.** Every download hits the origin. Bundles are served with
  `Cache-Control: public, max-age=31536000, immutable`, so a CDN or a proxy in
  front will cache them well if you add one.
- **Full-zip updates.** No delta support, so each update transfers the whole
  bundle. This is the plugin's default behaviour and the main thing worth
  implementing next if bandwidth matters.
- **No gradual rollout.** A release reaches everyone on the channel at once.
  Channels approximate staged rollout manually.
- **One operator.** Single API key, single admin login, no audit trail.

---

## Testing

`test/contract.sh` exercises the full plugin contract against a running server —
upload validation, checksum correctness, channel resolution, self-assignment
rules, rollback, `min_native` gating, path traversal and auth. 21 checks.

```bash
docker build -t capgo-selfhost:test .
docker run -d --name capgo-test -p 8099:8080 \
  -e API_KEY=testkey123 -e PUBLIC_URL=http://localhost:8099 capgo-selfhost:test
./test/contract.sh
```

It runs against the real container over HTTP rather than calling handlers
directly, which is what caught the `defaultChannel` bug described above — a
unit test of the same function would have asserted the wrong behaviour.

## Backups

Everything lives in `DATA_DIR`: `capgo.db` plus `bundles/`. Copy that directory
and you have copied the whole service. Losing it means devices stop receiving
updates; it does not brick any installed app, which keeps running the bundle it
already has.

## License

MIT.
