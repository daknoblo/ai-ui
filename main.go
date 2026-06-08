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
	apiKey := os.Getenv("AZURE_API_KEY") // Secret ausschließlich aus ENV.

	// Datenverzeichnisse anlegen.
	appDataDir := filepath.Join(dataDir, "appdata")
	if err := os.MkdirAll(appDataDir, 0o755); err != nil {
		return err
	}

	// Konfiguration laden (oder Default erzeugen).
	cfgStore := config.NewStore(filepath.Join(appDataDir, "config.json"), apiKey)
	if _, err := cfgStore.Load(); err != nil {
		return err
	}

	// SQLite-Datenbank im Datenpfad öffnen.
	dbPath := filepath.Join(dataDir, "ai-ui.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

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
