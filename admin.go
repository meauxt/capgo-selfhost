package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

//go:generate true

type adminData struct {
	Apps      []string
	App       string
	Channels  []Channel
	Bundles   []Bundle
	Devices   []Device
	Stats     []StatRow
	PublicURL string
	Flash     string
	Err       string
}

var adminTmpl = template.Must(template.New("admin").Funcs(template.FuncMap{
	"kb": func(n int64) string { return fmt.Sprintf("%.1f KB", float64(n)/1024) },
	"short": func(s string) string {
		if len(s) > 12 {
			return s[:12]
		}
		return s
	},
}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>capgo-selfhost</title>
<style>
:root{color-scheme:light dark}
body{font:14px/1.5 ui-sans-serif,system-ui,-apple-system,sans-serif;margin:0;padding:1.5rem;max-width:70rem}
h1{font-size:1.1rem;margin:0 0 .25rem}
h2{font-size:.95rem;margin:2rem 0 .5rem;padding-bottom:.25rem;border-bottom:1px solid #8884}
table{border-collapse:collapse;width:100%;margin:.5rem 0;display:block;overflow-x:auto}
th,td{text-align:left;padding:.35rem .6rem;border-bottom:1px solid #8883;white-space:nowrap}
th{font-weight:600;opacity:.7;font-size:.8rem;text-transform:uppercase;letter-spacing:.04em}
code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:.85em;opacity:.85}
form{display:flex;gap:.5rem;flex-wrap:wrap;align-items:center;margin:.5rem 0}
input,select,button{font:inherit;padding:.3rem .5rem;border:1px solid #8886;border-radius:6px;background:transparent;color:inherit}
button{cursor:pointer;background:#8882}
button:hover{background:#8884}
.flash{padding:.5rem .75rem;border-radius:6px;background:#2ecc7122;border:1px solid #2ecc7166;margin:.5rem 0}
.err{padding:.5rem .75rem;border-radius:6px;background:#e74c3c22;border:1px solid #e74c3c66;margin:.5rem 0}
.muted{opacity:.6}
nav a{margin-right:.75rem}
</style></head><body>
<h1>capgo-selfhost</h1>
<p class="muted">Self-hosted OTA updates for <code>@capgo/capacitor-updater</code> · <code>{{.PublicURL}}</code></p>

{{if .Flash}}<div class="flash">{{.Flash}}</div>{{end}}
{{if .Err}}<div class="err">{{.Err}}</div>{{end}}

<nav>{{range .Apps}}<a href="/admin?app={{.}}">{{.}}</a>{{end}}</nav>

{{if .App}}
<h2>Channels — {{.App}}</h2>
<table><tr><th>Channel</th><th>Serving</th><th>Public</th><th>Self-set</th><th></th></tr>
{{range .Channels}}<tr>
<td><code>{{.Name}}</code></td>
<td>{{if .Version}}<code>{{.Version}}</code>{{else}}<span class="muted">nothing</span>{{end}}</td>
<td>{{if .Public}}yes{{else}}—{{end}}</td>
<td>{{if .AllowSelfSet}}yes{{else}}—{{end}}</td>
<td><form method="post" action="/admin/channel-bundle">
  <input type="hidden" name="app" value="{{$.App}}"><input type="hidden" name="channel" value="{{.Name}}">
  <select name="version"><option value="">— unset —</option>{{range $.Bundles}}<option value="{{.Version}}">{{.Version}}</option>{{end}}</select>
  <button>Release</button></form></td>
</tr>{{end}}</table>

<form method="post" action="/admin/channel">
  <input type="hidden" name="app" value="{{.App}}">
  <input name="channel" placeholder="new channel name" required>
  <label><input type="checkbox" name="public" value="1"> public (default)</label>
  <label><input type="checkbox" name="allow_self_set" value="1"> self-assignable</label>
  <button>Create / update channel</button>
</form>

<h2>Bundles</h2>
<form method="post" action="/admin/upload" enctype="multipart/form-data">
  <input type="hidden" name="app" value="{{.App}}">
  <input name="version" placeholder="1.2.48" required>
  <input type="file" name="file" accept=".zip" required>
  <input name="min_native" placeholder="min native (optional)" size="18">
  <button>Upload bundle</button>
</form>
<table><tr><th>Version</th><th>Size</th><th>SHA-256</th><th>Min native</th><th>Uploaded</th><th></th></tr>
{{range .Bundles}}<tr>
<td><code>{{.Version}}</code></td><td>{{kb .Size}}</td><td><code>{{short .Checksum}}…</code></td>
<td>{{if .MinNative}}<code>{{.MinNative}}</code>{{else}}<span class="muted">—</span>{{end}}</td>
<td class="muted">{{.CreatedAt}}</td>
<td><form method="post" action="/admin/delete-bundle" onsubmit="return confirm('Delete {{.Version}}?')">
  <input type="hidden" name="app" value="{{$.App}}"><input type="hidden" name="version" value="{{.Version}}">
  <button>Delete</button></form></td>
</tr>{{else}}<tr><td colspan="6" class="muted">No bundles yet.</td></tr>{{end}}</table>

<h2>Devices <span class="muted">(last 200 seen)</span></h2>
<table><tr><th>Device</th><th>Platform</th><th>Bundle</th><th>Native</th><th>Channel</th><th>Last seen</th></tr>
{{range .Devices}}<tr><td><code>{{short .DeviceID}}…</code></td><td>{{.Platform}}</td>
<td><code>{{.VersionName}}</code></td><td><code>{{.VersionBuild}}</code></td>
<td>{{if .ChannelName}}{{.ChannelName}}{{else}}<span class="muted">default</span>{{end}}</td>
<td class="muted">{{.LastSeen}}</td></tr>
{{else}}<tr><td colspan="6" class="muted">No devices have checked in yet.</td></tr>{{end}}</table>

<h2>Recent events</h2>
<table><tr><th>Action</th><th>Bundle</th><th>Device</th><th>When</th></tr>
{{range .Stats}}<tr><td>{{.Action}}</td><td><code>{{.VersionName}}</code></td>
<td><code>{{short .DeviceID}}…</code></td><td class="muted">{{.CreatedAt}}</td></tr>
{{else}}<tr><td colspan="4" class="muted">Nothing yet.</td></tr>{{end}}</table>
{{else}}
<p class="muted">No apps yet. An app appears here the first time a device checks in,
or as soon as you upload a bundle for it via the API.</p>
{{end}}
</body></html>`))

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimSuffix(r.URL.Path, "/") {
	case "/admin/upload":
		s.adminUpload(w, r)
		return
	case "/admin/channel":
		s.adminChannel(w, r)
		return
	case "/admin/channel-bundle":
		app, ch := r.FormValue("app"), r.FormValue("channel")
		err := s.store.SetChannelBundle(app, ch, r.FormValue("version"))
		s.adminRedirect(w, r, app, fmt.Sprintf("Channel %s now serves %s", ch, r.FormValue("version")), err)
		return
	case "/admin/delete-bundle":
		app, version := r.FormValue("app"), r.FormValue("version")
		path, err := s.bundlePath(app, version)
		if err == nil {
			err = s.store.DeleteBundle(app, version)
			if err == nil {
				if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
					log.Printf("remove bundle: %v", rmErr)
				}
			}
		}
		s.adminRedirect(w, r, app, "Deleted "+version, err)
		return
	case "/admin":
		// fall through to render
	default:
		http.NotFound(w, r)
		return
	}

	apps, err := s.store.Apps()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	app := r.URL.Query().Get("app")
	if app == "" && len(apps) > 0 {
		app = apps[0]
	}
	data := adminData{
		Apps: apps, App: app, PublicURL: s.publicURL,
		Flash: r.URL.Query().Get("flash"), Err: r.URL.Query().Get("err"),
	}
	if app != "" {
		if data.Channels, err = s.store.Channels(app); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if data.Bundles, err = s.store.Bundles(app); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Devices, _ = s.store.Devices(app, 200)
		data.Stats, _ = s.store.RecentStats(app, 50)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTmpl.Execute(w, data); err != nil {
		log.Printf("render admin: %v", err)
	}
}

func (s *Server) adminRedirect(w http.ResponseWriter, r *http.Request, app, flash string, err error) {
	q := url.Values{"app": {app}}
	if err != nil {
		q.Set("err", err.Error())
	} else {
		q.Set("flash", flash)
	}
	http.Redirect(w, r, "/admin?"+q.Encode(), http.StatusSeeOther)
}

func (s *Server) adminChannel(w http.ResponseWriter, r *http.Request) {
	app, name := r.FormValue("app"), r.FormValue("channel")
	err := s.store.UpsertChannel(app, name, r.FormValue("public") == "1", r.FormValue("allow_self_set") == "1")
	s.adminRedirect(w, r, app, "Channel "+name+" saved", err)
}

func (s *Server) adminUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		s.adminRedirect(w, r, r.FormValue("app"), "", err)
		return
	}
	app, version := r.FormValue("app"), r.FormValue("version")
	if err := s.store.EnsureApp(app); err != nil {
		s.adminRedirect(w, r, app, "", err)
		return
	}
	if existing, err := s.store.BundleByVersion(app, version); err == nil && existing != nil {
		s.adminRedirect(w, r, app, "", fmt.Errorf("bundle %s already exists — bump the version or delete it first", version))
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		s.adminRedirect(w, r, app, "", err)
		return
	}
	defer file.Close()

	path, err := s.bundlePath(app, version)
	if err != nil {
		s.adminRedirect(w, r, app, "", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.adminRedirect(w, r, app, "", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		s.adminRedirect(w, r, app, "", err)
		return
	}
	defer os.Remove(tmp.Name())

	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(tmp, hash), file)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		s.adminRedirect(w, r, app, "", fmt.Errorf("write failed"))
		return
	}
	if err := validateBundleZip(tmp.Name()); err != nil {
		s.adminRedirect(w, r, app, "", err)
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		s.adminRedirect(w, r, app, "", err)
		return
	}
	if _, err := s.store.AddBundle(app, version, hex.EncodeToString(hash.Sum(nil)), r.FormValue("min_native"), size); err != nil {
		s.adminRedirect(w, r, app, "", err)
		return
	}
	s.adminRedirect(w, r, app, fmt.Sprintf("Uploaded %s (%.1f KB) — now pick a channel to release it on", version, float64(size)/1024), nil)
}
