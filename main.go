package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/logbuf"
	"github.com/daknoblo/ai-ui/internal/server"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// logs keeps the recent log lines in memory for the log page and owns the
// runtime log level.
var logs = logbuf.New(2000)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-healthcheck" || os.Args[1] == "healthcheck") {
		os.Exit(healthcheck())
	}

	logger := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, logs),
		&slog.HandlerOptions{Level: logs.LevelVar()}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Read the configuration from the environment.
	port := getenv("PORT", "8080")
	dataDir := getenv("DATA_DIR", "/appdata")
	keys := config.Keys{
		API:       os.Getenv("AZURE_API_KEY"),           // secret, environment only
		Embedding: os.Getenv("AZURE_EMBEDDING_API_KEY"), // optional; dedicated embedding key
		Image:     os.Getenv("AZURE_IMAGE_API_KEY"),     // optional; dedicated image key
		Search:    os.Getenv("SEARCH_API_KEY"),          // optional; web search key
	}
	healthCheckInterval := parseDurationEnv("HEALTHCHECK_INTERVAL", 60*time.Second)

	// Optional endpoint overrides from the environment. Values that are set
	// take precedence over the stored configuration and lock the matching UI
	// field.
	overrides := config.Overrides{
		Endpoint:            strings.TrimSpace(os.Getenv("AZURE_ENDPOINT")),
		ChatDeployment:      strings.TrimSpace(os.Getenv("AZURE_DEPLOYMENT")),
		ChatModels:          config.ParseModelList(os.Getenv("AZURE_MODELS")),
		APIVersion:          strings.TrimSpace(os.Getenv("AZURE_API_VERSION")),
		EmbeddingEndpoint:   strings.TrimSpace(os.Getenv("AZURE_EMBEDDING_ENDPOINT")),
		EmbeddingDeployment: strings.TrimSpace(os.Getenv("AZURE_EMBEDDING_DEPLOYMENT")),
		EmbeddingAPIVersion: strings.TrimSpace(os.Getenv("AZURE_EMBEDDING_API_VERSION")),
		ImageEndpoint:       strings.TrimSpace(os.Getenv("AZURE_IMAGE_ENDPOINT")),
		ImageDeployment:     strings.TrimSpace(os.Getenv("AZURE_IMAGE_DEPLOYMENT")),
		ImageAPIVersion:     strings.TrimSpace(os.Getenv("AZURE_IMAGE_API_VERSION")),
	}

	// Create the data directories. The application config lives in a sub
	// directory of DATA_DIR so the database files stay separate from it.
	appDataDir := filepath.Join(dataDir, "appdata")
	if err := os.MkdirAll(appDataDir, 0o750); err != nil {
		return err
	}

	// Load the configuration (or create the defaults).
	cfgStore := config.NewStore(filepath.Join(appDataDir, "config.json"), keys, overrides)
	cfg, err := cfgStore.Load()
	if err != nil {
		return err
	}
	logs.SetLevel(logbuf.ParseLevel(cfg.LogLevel))

	// Open the SQLite database in the data path.
	dbPath := filepath.Join(dataDir, "ai-ui.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	if err := store.Migrate(context.Background()); err != nil {
		return err
	}

	// Start the HTTP server.
	srv := server.New(cfgStore, store, logs)
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: document uploads may take a while and SSE
		// streams are long lived. IdleTimeout still reclaims idle keep-alive
		// connections, which would otherwise be held forever.
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	// Graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Verify the connection at start-up and monitor it periodically.
	go srv.Monitor(ctx, healthCheckInterval)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("server started", "addr", httpServer.Addr, "data_dir", dataDir)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown initiated")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// getenv returns the environment variable or fallback when it is unset/empty.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// healthcheck is the container health check. Distroless images ship neither a
// shell nor curl, so the binary probes its own /healthz endpoint.
func healthcheck() int {
	port := getenv("PORT", "8080")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// parseDurationEnv reads a duration from the environment (e.g. "30s", "2m").
// An invalid or missing value falls back to fallback. "0" or "off" disables the
// periodic check (the start-up check still runs).
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
		slog.Warn("invalid interval, using default", "key", key, "value", v)
		return fallback
	}
	return d
}
