package main

import (
	"archive/zip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// safeName rejects anything that could escape the bundles directory. app ids and
// versions arrive from the network, so this runs on both.
func safeName(s string) (string, error) {
	if s == "" || s == "." || s == ".." ||
		strings.ContainsAny(s, `/\`) || strings.Contains(s, "..") {
		return "", fmt.Errorf("unsafe name %q", s)
	}
	return s, nil
}

func (s *Server) bundlePath(appID, version string) (string, error) {
	a, err := safeName(appID)
	if err != nil {
		return "", err
	}
	v, err := safeName(version)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.dataDir, "bundles", a, v+".zip"), nil
}

func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if key == "" {
			key = r.Header.Get("X-API-Key")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.adminUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.adminPass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="capgo-selfhost"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleBundleDownload serves /bundles/{app_id}/{version}.zip. It is
// deliberately unauthenticated: the plugin downloads it with no credentials,
// and the contents are the same JS the app already ships.
func (s *Server) handleBundleDownload(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/bundles/")
	appID, file, ok := strings.Cut(rest, "/")
	if !ok || !strings.HasSuffix(file, ".zip") {
		http.NotFound(w, r)
		return
	}
	path, err := s.bundlePath(appID, strings.TrimSuffix(file, ".zip"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "stat failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	// Bundles are immutable per version, so they cache forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, file, st.ModTime(), f)
}

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/"), "/"), "/")
	// Routes:
	//   POST   /api/apps/{app}/bundles                      upload
	//   GET    /api/apps/{app}/bundles                      list
	//   DELETE /api/apps/{app}/bundles/{version}            delete
	//   GET    /api/apps/{app}/channels                     list
	//   POST   /api/apps/{app}/channels/{channel}           create/update
	//   POST   /api/apps/{app}/channels/{channel}/bundle    point at a version
	//   GET    /api/apps/{app}/devices
	if len(parts) < 3 || parts[0] != "apps" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
		return
	}
	appID := parts[1]
	if _, err := safeName(appID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_app_id"})
		return
	}
	if err := s.store.EnsureApp(appID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}

	switch {
	case parts[2] == "bundles" && len(parts) == 3 && r.Method == http.MethodPost:
		s.apiUploadBundle(w, r, appID)
	case parts[2] == "bundles" && len(parts) == 3 && r.Method == http.MethodGet:
		list, err := s.store.Bundles(appID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		out := make([]map[string]any, 0, len(list))
		for _, b := range list {
			out = append(out, map[string]any{
				"version": b.Version, "checksum": b.Checksum, "size": b.Size,
				"min_native": b.MinNative, "created_at": b.CreatedAt,
				"url": s.bundleURL(appID, b.Version),
			})
		}
		writeJSON(w, http.StatusOK, out)
	case parts[2] == "bundles" && len(parts) == 4 && r.Method == http.MethodDelete:
		s.apiDeleteBundle(w, appID, parts[3])
	case parts[2] == "channels" && len(parts) == 3 && r.Method == http.MethodGet:
		chans, err := s.store.Channels(appID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		out := make([]map[string]any, 0, len(chans))
		for _, c := range chans {
			out = append(out, map[string]any{
				"id": c.ID, "name": c.Name, "version": c.Version,
				"public": c.Public, "allow_self_set": c.AllowSelfSet,
			})
		}
		writeJSON(w, http.StatusOK, out)
	case parts[2] == "channels" && len(parts) == 4 && r.Method == http.MethodPost:
		public := r.URL.Query().Get("public") == "true"
		allowSelfSet := r.URL.Query().Get("allow_self_set") == "true"
		if err := s.store.UpsertChannel(appID, parts[3], public, allowSelfSet); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case parts[2] == "channels" && len(parts) == 5 && parts[4] == "bundle" && r.Method == http.MethodPost:
		version := r.URL.Query().Get("version")
		if err := s.store.SetChannelBundle(appID, parts[3], version); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		log.Printf("channel %s/%s -> %s", appID, parts[3], version)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case parts[2] == "devices" && r.Method == http.MethodGet:
		devices, err := s.store.Devices(appID, 200)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
			return
		}
		writeJSON(w, http.StatusOK, devices)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	}
}

func (s *Server) apiUploadBundle(w http.ResponseWriter, r *http.Request, appID string) {
	// 512 MB ceiling; real bundles are single-digit MB.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_multipart", "message": err.Error()})
		return
	}
	version := r.FormValue("version")
	if _, err := safeName(version); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_version"})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_file"})
		return
	}
	defer file.Close()

	if existing, err := s.store.BundleByVersion(appID, version); err == nil && existing != nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":   "version_exists",
			"message": fmt.Sprintf("bundle %s already uploaded; bump the version or delete it first", version)})
		return
	}

	path, err := s.bundlePath(appID, version)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_path"})
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "mkdir_failed"})
		return
	}

	// Write to a temp file first so a failed upload never leaves a bundle that
	// devices could download half of.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "temp_failed"})
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hash), file)
	closeErr := tmp.Close()
	if err != nil || closeErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write_failed"})
		return
	}
	if err := validateBundleZip(tmpName); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_bundle", "message": err.Error()})
		return
	}
	checksum := hex.EncodeToString(hash.Sum(nil))

	if err := os.Rename(tmpName, path); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "rename_failed"})
		return
	}
	if _, err := s.store.AddBundle(appID, version, checksum, r.FormValue("min_native"), size); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "db_failed", "message": err.Error()})
		return
	}

	// Optional one-shot release: upload and point a channel at it together.
	if ch := r.FormValue("channel"); ch != "" {
		if err := s.store.SetChannelBundle(appID, ch, version); err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"version": version, "checksum": checksum, "size": size,
				"warning": "uploaded but channel not set: " + err.Error()})
			return
		}
	}

	log.Printf("uploaded %s %s (%d bytes, sha256 %s)", appID, version, size, checksum)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version, "checksum": checksum, "size": size,
		"url": s.bundleURL(appID, version),
	})
}

// validateBundleZip catches the most common release mistake: zipping the dist
// folder itself, so every path is prefixed with "dist/" and the plugin finds no
// index.html at the root.
func validateBundleZip(path string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("not a valid zip: %w", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "index.html" {
			return nil
		}
	}
	var top string
	if len(zr.File) > 0 {
		top = strings.SplitN(zr.File[0].Name, "/", 2)[0]
	}
	return fmt.Errorf(
		"no index.html at the zip root (top-level entry is %q) — zip the *contents* of dist, not the dist folder", top)
}

func (s *Server) apiDeleteBundle(w http.ResponseWriter, appID, version string) {
	path, err := s.bundlePath(appID, version)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_version"})
		return
	}
	if err := s.store.DeleteBundle(appID, version); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("remove bundle file: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
