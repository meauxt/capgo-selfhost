package main

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type Store struct{ db *sql.DB }

type Bundle struct {
	ID        int64
	AppID     string
	Version   string
	Checksum  string
	Size      int64
	MinNative string
	CreatedAt string
}

type Channel struct {
	ID           int64
	AppID        string
	Name         string
	BundleID     sql.NullInt64
	Public       bool
	AllowSelfSet bool
	// Version is the bundle version the channel points at, empty if unset.
	Version string
}

type Device struct {
	DeviceID     string
	ChannelName  string
	CustomID     string
	Platform     string
	VersionName  string
	VersionBuild string
	LastSeen     string
}

func openStore(path string) (*Store, error) {
	// _time_format=sqlite keeps timestamps readable; busy_timeout avoids
	// "database is locked" when the admin UI writes during an update check.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite writes serialize anyway; a single connection sidesteps lock churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// EnsureApp creates the app row and its default public channel on first sight,
// so a device checking in for an unknown app does not 500.
func (s *Store) EnsureApp(appID string) error {
	if appID == "" {
		return errors.New("empty app_id")
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO apps (app_id, created_at) VALUES (?, ?)`, appID, now()); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO channels (app_id, name, public, allow_self_set) VALUES (?, ?, 1, 0)`,
		appID, defaultChannelName)
	return err
}

func (s *Store) Apps() ([]string, error) {
	rows, err := s.db.Query(`SELECT app_id FROM apps ORDER BY app_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) AddBundle(appID, version, checksum, minNative string, size int64) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO bundles (app_id, version, checksum, size, min_native, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		appID, version, checksum, size, minNative, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Bundles(appID string) ([]Bundle, error) {
	rows, err := s.db.Query(
		`SELECT id, app_id, version, checksum, size, min_native, created_at
		 FROM bundles WHERE app_id = ? ORDER BY id DESC`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bundle
	for rows.Next() {
		var b Bundle
		if err := rows.Scan(&b.ID, &b.AppID, &b.Version, &b.Checksum, &b.Size, &b.MinNative, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) BundleByVersion(appID, version string) (*Bundle, error) {
	var b Bundle
	err := s.db.QueryRow(
		`SELECT id, app_id, version, checksum, size, min_native, created_at
		 FROM bundles WHERE app_id = ? AND version = ?`, appID, version).
		Scan(&b.ID, &b.AppID, &b.Version, &b.Checksum, &b.Size, &b.MinNative, &b.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &b, err
}

func (s *Store) DeleteBundle(appID, version string) error {
	_, err := s.db.Exec(`DELETE FROM bundles WHERE app_id = ? AND version = ?`, appID, version)
	return err
}

const channelSelect = `SELECT c.id, c.app_id, c.name, c.bundle_id, c.public, c.allow_self_set,
       COALESCE(b.version, '')
  FROM channels c LEFT JOIN bundles b ON b.id = c.bundle_id`

func scanChannel(sc interface{ Scan(...any) error }) (*Channel, error) {
	var c Channel
	if err := sc.Scan(&c.ID, &c.AppID, &c.Name, &c.BundleID, &c.Public, &c.AllowSelfSet, &c.Version); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) Channels(appID string) ([]Channel, error) {
	rows, err := s.db.Query(channelSelect+` WHERE c.app_id = ? ORDER BY c.name`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *Store) Channel(appID, name string) (*Channel, error) {
	c, err := scanChannel(s.db.QueryRow(channelSelect+` WHERE c.app_id = ? AND c.name = ?`, appID, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *Store) ChannelByID(id int64) (*Channel, error) {
	c, err := scanChannel(s.db.QueryRow(channelSelect+` WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// PublicChannel is the fallback for devices with no explicit assignment.
func (s *Store) PublicChannel(appID string) (*Channel, error) {
	c, err := scanChannel(s.db.QueryRow(
		channelSelect+` WHERE c.app_id = ? AND c.public = 1 ORDER BY c.id LIMIT 1`, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *Store) UpsertChannel(appID, name string, public, allowSelfSet bool) error {
	_, err := s.db.Exec(
		`INSERT INTO channels (app_id, name, public, allow_self_set) VALUES (?, ?, ?, ?)
		 ON CONFLICT (app_id, name) DO UPDATE SET public = excluded.public,
		                                          allow_self_set = excluded.allow_self_set`,
		appID, name, public, allowSelfSet)
	if err != nil {
		return err
	}
	if public {
		// Exactly one public channel per app, otherwise fallback resolution is
		// ambiguous and devices land wherever the id ordering happens to put them.
		_, err = s.db.Exec(`UPDATE channels SET public = 0 WHERE app_id = ? AND name != ?`, appID, name)
	}
	return err
}

func (s *Store) DeleteChannel(appID, name string) error {
	_, err := s.db.Exec(`DELETE FROM channels WHERE app_id = ? AND name = ?`, appID, name)
	return err
}

// SetChannelBundle points a channel at a bundle version, or clears it when
// version is empty.
func (s *Store) SetChannelBundle(appID, channel, version string) error {
	if version == "" {
		_, err := s.db.Exec(`UPDATE channels SET bundle_id = NULL WHERE app_id = ? AND name = ?`, appID, channel)
		return err
	}
	b, err := s.BundleByVersion(appID, version)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("no bundle %s for app %s", version, appID)
	}
	res, err := s.db.Exec(`UPDATE channels SET bundle_id = ? WHERE app_id = ? AND name = ?`, b.ID, appID, channel)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no channel %q for app %s", channel, appID)
	}
	return nil
}

func (s *Store) UpsertDevice(i AppInfos) error {
	_, err := s.db.Exec(
		`INSERT INTO devices (app_id, device_id, custom_id, platform, version_name,
		                      version_build, plugin_version, version_os, is_emulator, is_prod, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (app_id, device_id) DO UPDATE SET
		   custom_id = excluded.custom_id, platform = excluded.platform,
		   version_name = excluded.version_name, version_build = excluded.version_build,
		   plugin_version = excluded.plugin_version, version_os = excluded.version_os,
		   is_emulator = excluded.is_emulator, is_prod = excluded.is_prod,
		   last_seen = excluded.last_seen`,
		i.AppID, i.DeviceID, i.CustomID, i.Platform, i.VersionName,
		i.VersionBuild, i.PluginVersion, i.VersionOS, i.IsEmulator, i.IsProd, now())
	return err
}

// DeviceChannel returns the channel a device was explicitly pinned to, or nil.
func (s *Store) DeviceChannel(appID, deviceID string) (*Channel, error) {
	var id sql.NullInt64
	err := s.db.QueryRow(`SELECT channel_id FROM devices WHERE app_id = ? AND device_id = ?`,
		appID, deviceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !id.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.ChannelByID(id.Int64)
}

func (s *Store) SetDeviceChannel(appID, deviceID string, channelID *int64) error {
	var v any
	if channelID != nil {
		v = *channelID
	}
	_, err := s.db.Exec(`UPDATE devices SET channel_id = ? WHERE app_id = ? AND device_id = ?`,
		v, appID, deviceID)
	return err
}

func (s *Store) Devices(appID string, limit int) ([]Device, error) {
	rows, err := s.db.Query(
		`SELECT d.device_id, COALESCE(c.name, ''), d.custom_id, d.platform,
		        d.version_name, d.version_build, d.last_seen
		   FROM devices d LEFT JOIN channels c ON c.id = d.channel_id
		  WHERE d.app_id = ? ORDER BY d.last_seen DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.DeviceID, &d.ChannelName, &d.CustomID, &d.Platform,
			&d.VersionName, &d.VersionBuild, &d.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) AddStat(i AppInfos, action, versionName, oldVersionName string) error {
	_, err := s.db.Exec(
		`INSERT INTO stats (app_id, device_id, action, version_name, old_version_name, platform, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		i.AppID, i.DeviceID, action, versionName, oldVersionName, i.Platform, now())
	return err
}

type StatRow struct {
	Action      string
	VersionName string
	DeviceID    string
	CreatedAt   string
}

func (s *Store) RecentStats(appID string, limit int) ([]StatRow, error) {
	rows, err := s.db.Query(
		`SELECT action, version_name, device_id, created_at FROM stats
		  WHERE app_id = ? ORDER BY id DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatRow
	for rows.Next() {
		var r StatRow
		if err := rows.Scan(&r.Action, &r.VersionName, &r.DeviceID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneStats keeps the table from growing without bound on a small box.
func (s *Store) PruneStats(keep int) error {
	_, err := s.db.Exec(
		`DELETE FROM stats WHERE id NOT IN (SELECT id FROM stats ORDER BY id DESC LIMIT ?)`, keep)
	return err
}

// compareSemver returns -1, 0 or 1. Non-numeric or missing parts sort as 0, and
// any pre-release suffix is ignored — enough to answer "is the native build new
// enough for this bundle", which is all it is used for.
func compareSemver(a, b string) int {
	split := func(v string) []int {
		v = strings.SplitN(strings.TrimSpace(v), "-", 2)[0]
		v = strings.SplitN(v, "+", 2)[0]
		parts := strings.Split(v, ".")
		out := make([]int, 3)
		for i := 0; i < 3 && i < len(parts); i++ {
			n, err := strconv.Atoi(parts[i])
			if err != nil {
				return out
			}
			out[i] = n
		}
		return out
	}
	x, y := split(a), split(b)
	for i := 0; i < 3; i++ {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
