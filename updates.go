package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
)

const defaultChannelName = "production"

// flexString accepts both "1.2.3" and 123 — the plugin sends version_code as a
// string on Android but platforms have historically differed, and a type
// mismatch here would fail the whole update check.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	if string(b) == "null" {
		*f = ""
		return nil
	}
	*f = flexString(strings.Trim(string(b), `"`))
	return nil
}

func (f flexString) String() string { return string(f) }

// AppInfos is the payload @capgo/capacitor-updater posts to every endpoint.
// Field names mirror CapgoUpdater.createInfoObject() in the plugin.
type AppInfos struct {
	Platform       string     `json:"platform"`
	DeviceID       string     `json:"device_id"`
	AppID          string     `json:"app_id"`
	CustomID       string     `json:"custom_id"`
	VersionBuild   flexString `json:"version_build"`
	VersionCode    flexString `json:"version_code"`
	VersionOS      flexString `json:"version_os"`
	VersionName    string     `json:"version_name"`
	PluginVersion  string     `json:"plugin_version"`
	IsEmulator     bool       `json:"is_emulator"`
	IsProd         bool       `json:"is_prod"`
	InstallSource  string     `json:"install_source"`
	DefaultChannel string     `json:"defaultChannel"`

	// Only present on channel and stats calls.
	Channel        string `json:"channel"`
	Action         string `json:"action"`
	OldVersionName string `json:"old_version_name"`
}

func (i AppInfos) versionBuild() string { return i.VersionBuild.String() }
func (i AppInfos) versionOS() string    { return i.VersionOS.String() }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

// decodeInfos reads AppInfos from a JSON body, or from the query string, which
// is how the plugin sends the "list channels" GET.
func decodeInfos(r *http.Request) (AppInfos, error) {
	var i AppInfos
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		i.AppID = q.Get("app_id")
		i.DeviceID = q.Get("device_id")
		i.Platform = q.Get("platform")
		i.VersionName = q.Get("version_name")
		i.VersionBuild = flexString(q.Get("version_build"))
		i.CustomID = q.Get("custom_id")
		i.DefaultChannel = q.Get("defaultChannel")
		i.Channel = q.Get("channel")
	} else {
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(&i); err != nil {
			return i, fmt.Errorf("bad json: %w", err)
		}
	}
	if i.AppID == "" {
		return i, errors.New("missing app_id")
	}
	return i, nil
}

// resolveChannel picks the channel a device should be served from:
// explicit device assignment first, then a self-set defaultChannel the app
// declares in its config, then the app's public channel.
func (s *Server) resolveChannel(i AppInfos) (*Channel, error) {
	if i.DeviceID != "" {
		c, err := s.store.DeviceChannel(i.AppID, i.DeviceID)
		if err != nil {
			return nil, err
		}
		if c != nil {
			return c, nil
		}
	}
	if i.DefaultChannel != "" {
		c, err := s.store.Channel(i.AppID, i.DefaultChannel)
		if err != nil {
			return nil, err
		}
		// A channel the app asks for by name is only honoured if it opted in.
		if c != nil && (c.Public || c.AllowSelfSet) {
			return c, nil
		}
	}
	return s.store.PublicChannel(i.AppID)
}

func (s *Server) bundleURL(appID, version string) string {
	return fmt.Sprintf("%s/bundles/%s/%s.zip",
		strings.TrimRight(s.publicURL, "/"), url.PathEscape(appID), url.PathEscape(version))
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	i, err := decodeInfos(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_request", "message": err.Error()})
		return
	}
	if err := s.store.EnsureApp(i.AppID); err != nil {
		log.Printf("ensure app %s: %v", i.AppID, err)
	}
	if i.DeviceID != "" {
		if err := s.store.UpsertDevice(i); err != nil {
			log.Printf("upsert device: %v", err)
		}
	}

	ch, err := s.resolveChannel(i)
	if err != nil {
		log.Printf("resolve channel: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	if ch == nil || !ch.BundleID.Valid {
		writeJSON(w, http.StatusOK, map[string]string{"message": "No new version available"})
		return
	}
	b, err := s.store.BundleByVersion(i.AppID, ch.Version)
	if err != nil || b == nil {
		writeJSON(w, http.StatusOK, map[string]string{"message": "No new version available"})
		return
	}
	// Serving the same version again would make the plugin re-download and
	// reinstall the bundle it is already running.
	if b.Version == i.VersionName {
		writeJSON(w, http.StatusOK, map[string]string{"message": "No new version available"})
		return
	}
	// A bundle can require a minimum native shell — an OTA update cannot ship
	// new native code, so serving it to an older binary breaks the app.
	if b.MinNative != "" && i.versionBuild() != "" && compareSemver(i.versionBuild(), b.MinNative) < 0 {
		writeJSON(w, http.StatusOK, map[string]string{
			"message": fmt.Sprintf("Native app %s is older than required %s", i.versionBuild(), b.MinNative)})
		return
	}

	log.Printf("update %s %s: %s -> %s (channel %s)", i.AppID, i.Platform, i.VersionName, b.Version, ch.Name)
	writeJSON(w, http.StatusOK, map[string]string{
		"version":  b.Version,
		"url":      s.bundleURL(i.AppID, b.Version),
		"checksum": b.Checksum,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	i, err := decodeInfos(r)
	if err != nil {
		// Stats are best-effort; never make the plugin retry on our account.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if err := s.store.AddStat(i, i.Action, i.VersionName, i.OldVersionName); err != nil {
		log.Printf("add stat: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleChannelSelf implements the four verbs the plugin uses against
// channelUrl: POST set, PUT get, DELETE unset, GET list.
func (s *Server) handleChannelSelf(w http.ResponseWriter, r *http.Request) {
	i, err := decodeInfos(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid_request", "message": err.Error()})
		return
	}
	if err := s.store.EnsureApp(i.AppID); err != nil {
		log.Printf("ensure app: %v", err)
	}

	switch r.Method {
	case http.MethodGet:
		chans, err := s.store.Channels(i.AppID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		// The plugin expects a bare JSON array here, not an object.
		out := make([]map[string]any, 0, len(chans))
		for _, c := range chans {
			if !c.Public && !c.AllowSelfSet {
				continue
			}
			out = append(out, map[string]any{
				"id": c.ID, "name": c.Name, "public": c.Public, "allow_self_set": c.AllowSelfSet,
			})
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPut:
		ch, err := s.resolveChannel(i)
		if err != nil || ch == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"error": "channel_not_found", "message": "no channel for this device"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"channel": ch.Name, "status": "ok", "allowSet": ch.AllowSelfSet,
		})

	case http.MethodPost:
		if i.DeviceID == "" {
			writeJSON(w, http.StatusOK, map[string]any{
				"error": "missing_device_id", "message": "device_id required"})
			return
		}
		ch, err := s.store.Channel(i.AppID, i.Channel)
		if err != nil || ch == nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"error": "channel_not_found", "message": "unknown channel " + i.Channel})
			return
		}
		if !ch.AllowSelfSet {
			writeJSON(w, http.StatusOK, map[string]any{
				"error": "channel_not_self_assignable", "message": "channel " + ch.Name + " is not self-assignable"})
			return
		}
		if err := s.store.UpsertDevice(i); err != nil {
			log.Printf("upsert device: %v", err)
		}
		if ch.Public {
			// Pinning to the public channel is the same as no override; telling
			// the plugin to unset keeps its local state honest.
			if err := s.store.SetDeviceChannel(i.AppID, i.DeviceID, nil); err != nil {
				log.Printf("clear device channel: %v", err)
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "unset": true})
			return
		}
		if err := s.store.SetDeviceChannel(i.AppID, i.DeviceID, &ch.ID); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"error": "server_error", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})

	case http.MethodDelete:
		if i.DeviceID != "" {
			if err := s.store.SetDeviceChannel(i.AppID, i.DeviceID, nil); err != nil {
				log.Printf("clear device channel: %v", err)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
	}
}
