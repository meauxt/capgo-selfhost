CREATE TABLE IF NOT EXISTS apps (
    app_id     TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS bundles (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id     TEXT NOT NULL,
    version    TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    size       INTEGER NOT NULL,
    min_native TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE (app_id, version)
);

CREATE TABLE IF NOT EXISTS channels (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id         TEXT NOT NULL,
    name           TEXT NOT NULL,
    bundle_id      INTEGER REFERENCES bundles (id) ON DELETE SET NULL,
    public         INTEGER NOT NULL DEFAULT 0,
    allow_self_set INTEGER NOT NULL DEFAULT 0,
    UNIQUE (app_id, name)
);

CREATE TABLE IF NOT EXISTS devices (
    app_id         TEXT NOT NULL,
    device_id      TEXT NOT NULL,
    channel_id     INTEGER REFERENCES channels (id) ON DELETE SET NULL,
    custom_id      TEXT NOT NULL DEFAULT '',
    platform       TEXT NOT NULL DEFAULT '',
    version_name   TEXT NOT NULL DEFAULT '',
    version_build  TEXT NOT NULL DEFAULT '',
    plugin_version TEXT NOT NULL DEFAULT '',
    version_os     TEXT NOT NULL DEFAULT '',
    is_emulator    INTEGER NOT NULL DEFAULT 0,
    is_prod        INTEGER NOT NULL DEFAULT 1,
    last_seen      TEXT NOT NULL,
    PRIMARY KEY (app_id, device_id)
);

CREATE TABLE IF NOT EXISTS stats (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id           TEXT NOT NULL,
    device_id        TEXT NOT NULL,
    action           TEXT NOT NULL,
    version_name     TEXT NOT NULL DEFAULT '',
    old_version_name TEXT NOT NULL DEFAULT '',
    platform         TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS stats_app_created ON stats (app_id, created_at DESC);
CREATE INDEX IF NOT EXISTS devices_last_seen ON devices (app_id, last_seen DESC);
