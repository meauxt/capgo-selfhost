package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Server struct {
	store     *Store
	dataDir   string
	publicURL string
	apiKey    string
	adminUser string
	adminPass string
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	dataDir := env("DATA_DIR", "./data")
	publicURL := strings.TrimRight(env("PUBLIC_URL", "http://localhost:8080"), "/")
	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		log.Fatal("API_KEY is required — it protects bundle uploads and the admin UI")
	}
	if strings.HasPrefix(publicURL, "http://") && !strings.Contains(publicURL, "localhost") {
		// iOS and Android both refuse plain-HTTP bundle downloads, so this would
		// look healthy in curl and fail silently on every device.
		log.Printf("WARNING: PUBLIC_URL is not HTTPS (%s); devices will refuse to download bundles", publicURL)
	}

	if err := os.MkdirAll(filepath.Join(dataDir, "bundles"), 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	store, err := openStore(filepath.Join(dataDir, "capgo.db"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	s := &Server{
		store:     store,
		dataDir:   dataDir,
		publicURL: publicURL,
		apiKey:    apiKey,
		adminUser: env("ADMIN_USER", "admin"),
		adminPass: env("ADMIN_PASSWORD", apiKey),
	}

	mux := http.NewServeMux()
	// Endpoints the plugin talks to. Paths match plugin.capgo.app so the app
	// config only has to swap the host.
	mux.HandleFunc("/updates", s.handleUpdates)
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/channel_self", s.handleChannelSelf)
	mux.HandleFunc("/bundles/", s.handleBundleDownload)

	// Management API and UI.
	mux.HandleFunc("/api/", s.requireAPIKey(s.handleAPI))
	mux.HandleFunc("/admin", s.requireAdmin(s.handleAdmin))
	mux.HandleFunc("/admin/", s.requireAdmin(s.handleAdmin))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/admin", http.StatusFound)
	})

	go s.pruneLoop()

	addr := ":" + env("PORT", "8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
		// Bundle uploads can be tens of MB over a slow link.
		WriteTimeout: 5 * time.Minute,
		ReadTimeout:  5 * time.Minute,
	}

	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		close(idle)
	}()

	log.Printf("capgo-selfhost listening on %s, public URL %s, data %s", addr, publicURL, dataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
	<-idle
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

// pruneLoop keeps the stats table bounded; kitsune has 8 GB of disk to spare,
// not 80.
func (s *Server) pruneLoop() {
	for {
		if err := s.store.PruneStats(50000); err != nil {
			log.Printf("prune stats: %v", err)
		}
		time.Sleep(24 * time.Hour)
	}
}
