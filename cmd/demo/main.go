// Command demo runs the application against a stub backend with seeded content.
// It needs no API key and no Azure resources, which makes it the basis for the
// automated documentation screenshots as well as a quick way to try the UI:
//
//	go run ./cmd/demo            # http://localhost:8080
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/daknoblo/ai-ui/internal/demo"
	"github.com/daknoblo/ai-ui/internal/logbuf"
	"github.com/daknoblo/ai-ui/internal/server"
)

func main() {
	port := flag.String("port", "8080", "HTTP port of the demo instance")
	dataDir := flag.String("data", filepath.Join(os.TempDir(), "ai-ui-demo"), "data path of the demo instance")
	lang := flag.String("lang", "en", "interface language of the demo content (en or de)")
	reset := flag.Bool("reset", false, "delete the database in the data path before seeding")
	flag.Parse()

	if err := run(*port, *dataDir, *lang, *reset); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(port, dataDir, lang string, reset bool) error {
	logs := logbuf.New(2000)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stdout, logs),
		&slog.HandlerOptions{Level: logs.LevelVar()})))

	if reset {
		if err := resetData(dataDir); err != nil {
			return err
		}
	}

	backend, err := demo.StartBackend(lang)
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfgStore, store, idx, err := demo.Setup(ctx, dataDir, lang, backend.URL())
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	srv := server.New(cfgStore, store, logs)
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go srv.Monitor(ctx, 0)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("demo started", "addr", httpServer.Addr, "data_dir", dataDir,
			"lang", idx.Lang, "chats", len(idx.Chats))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// resetData removes the database and the stored settings of a previous run so
// the demo starts from the same state every time. Only the files the demo
// itself creates are deleted.
func resetData(dataDir string) error {
	targets := []string{
		filepath.Join(dataDir, "ai-ui.db"),
		filepath.Join(dataDir, "ai-ui.db-wal"),
		filepath.Join(dataDir, "ai-ui.db-shm"),
		filepath.Join(dataDir, "appdata", "config.json"),
		filepath.Join(dataDir, demo.IndexFile),
	}
	for _, path := range targets {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
