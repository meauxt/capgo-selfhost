# capgo-selfhost

A small self-hosted update server for
[`@capgo/capacitor-updater`](https://github.com/Cap-go/capacitor-updater).

The plugin is open source and its self-hosted mode just needs an HTTP endpoint
that answers "is there a newer bundle?". This is that endpoint — one Go binary,
one SQLite file, one directory of zips. It is **not** a rebuild of Capgo Cloud:
no analytics, no org/team management, no encryption or delta updates.

## Why this exists

Self-hosting the real Capgo console means running self-hosted Supabase, their
Deno edge functions and a Vue frontend — and the upstream repo ships no
Dockerfile or compose file for it. If all you want is to ship OTA updates from
your own box, this is a couple of orders of magnitude less machinery.

## What it implements

| Endpoint | Method | Purpose |
|---|---|---|
| `/updates` | POST | Update check. Returns `{version, url, checksum}` or `{message}` |
| `/stats` | POST | Records plugin events (`update`, `download_fail`, …) |
| `/channel_self` | GET | Lists channels the device may self-assign to |
| `/channel_self` | PUT | Reports the device's current channel |
| `/channel_self` | POST | Self-assigns the device to a channel |
| `/channel_self` | DELETE | Clears the assignment |
| `/bundles/{app}/{version}.zip` | GET | Serves the bundle (unauthenticated, like Capgo Cloud) |
| `/admin` | GET | Web UI, HTTP basic auth |
| `/api/…` | — | Management API, `Authorization: Bearer $API_KEY` |

The request and response shapes were taken from the plugin's Android source
(`CapgoUpdater.createInfoObject`, `CapacitorUpdaterPlugin`), not from the docs.

Not implemented: bundle encryption (`sessionKey`), delta/manifest updates,
organisations. Devices fall back to full-zip downloads, which is the plugin's
default anyway.

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
download a bundle over plain HTTP, and the plugin swallows the failure. The
server logs a warning at boot if it sees a non-HTTPS public URL.

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

## Releasing

```bash
npm run build
CAPGO_URL=https://updates.example.com CAPGO_KEY=… \
  ./release.sh com.example.app 1.2.48 ./dist production
```

Or upload through `/admin` and pick a channel from the dropdown.

Version strings are yours to choose but must be unique per app; re-uploading an
existing version is rejected rather than silently replacing a bundle devices
may already have downloaded.

## Channels

Every app gets a `production` channel on first contact, marked public — that is
the fallback for any device with no explicit assignment. Additional channels
(`beta`, `staging`) are created from the admin UI or the API.

- **public**: the default channel for unassigned devices. Exactly one per app.
- **allow_self_set**: the app may move itself onto the channel at runtime via
  `CapacitorUpdater.setChannel()`. Leave it off for channels you assign yourself.

Pointing a channel at an older bundle is a valid rollback: the server serves
whatever the channel points at, and only skips when the device already runs
that exact version.

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

## Testing

`test/contract.sh` exercises the full plugin contract against a running server
(upload, checksum, channel resolution, self-assignment, rollback, path
traversal, auth):

```bash
docker build -t capgo-selfhost:test .
docker run -d --name capgo-test -p 8099:8080 \
  -e API_KEY=testkey123 -e PUBLIC_URL=http://localhost:8099 capgo-selfhost:test
./test/contract.sh
```

## Backups

Everything lives in `DATA_DIR`: `capgo.db` plus `bundles/`. Copy that directory
and you have copied the whole service. Losing it means devices stop receiving
updates; it does not brick any installed app, which keeps running the bundle it
already has.

## License

MIT.
