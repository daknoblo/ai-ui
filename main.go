package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/server"
	"github.com/daknoblo/ai-ui/internal/storage"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Konfiguration aus Umgebung lesen.
	port := getenv("PORT", "8080")
	dataDir := getenv("DATA_DIR", "/data")
	apiKey := os.Getenv("AZURE_API_KEY")                    // Secret ausschließlich aus ENV.
	embeddingAPIKey := os.Getenv("AZURE_EMBEDDING_API_KEY") // optional; eigener Key für Embeddings.
	searchAPIKey := os.Getenv("SEARCH_API_KEY")             // optional; Key für die Websuche.
	healthCheckInterval := parseDurationEnv("HEALTHCHECK_INTERVAL", 60*time.Second)

	// Datenverzeichnisse anlegen.
	appDataDir := filepath.Join(dataDir, "appdata")
	if err := os.MkdirAll(appDataDir, 0o755); err != nil {
		return err
	}

	// Konfiguration laden (oder Default erzeugen).
	cfgStore := config.NewStore(filepath.Join(appDataDir, "config.json"), apiKey, embeddingAPIKey, searchAPIKey)
	if _, err := cfgStore.Load(); err != nil {
		return err
	}

	// SQLite-Datenbank im Datenpfad öffnen.
	dbPath := filepath.Join(dataDir, "ai-ui.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if err := store.Migrate(context.Background()); err != nil {
		return err
	}

	// HTTP-Server starten.
	srv := server.New(cfgStore, store)
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Kein WriteTimeout: SSE-Streams sind langlebig.
	}

	// Graceful Shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Verbindung beim Start verifizieren und periodisch überwachen.
	go srv.Monitor(ctx, healthCheckInterval)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("server gestartet", "addr", httpServer.Addr, "data_dir", dataDir)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown eingeleitet")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseDurationEnv liest eine Dauer aus der Umgebung (z.B. "30s", "2m").
// Bei ungültigem oder fehlendem Wert wird fallback verwendet. "0" oder "off"
// deaktiviert den periodischen Check (nur Start-Prüfung).
func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	if v == "0" || v == "off" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		slog.Warn("ungültiges intervall, nutze default", "key", key, "wert", v)
		return fallback
	}
	return d
}
