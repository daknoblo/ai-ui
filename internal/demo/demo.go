// Package demo builds a self-contained demo instance of the application: a stub
// of the Azure-compatible backend plus seeded chats, documents, images and token
// statistics. It exists so the interface can be explored - and captured for the
// documentation - without any cloud resources or API keys.
package demo

import (
	"context"
	"os"
	"path/filepath"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/i18n"
	"github.com/daknoblo/ai-ui/internal/storage"
)

// Models are the deployments the demo offers in the model picker.
var Models = []string{"gpt-5.5", "claude-opus-4-7", "o4-mini"}

// searchEndpoint is a placeholder so the web search feature is visible in the
// interface. The stub backend does not serve searches: the application refuses
// loopback destinations for user supplied URLs, which is exactly the protection
// that should stay in place.
const searchEndpoint = "https://searxng.example.com"

// Config returns the configuration a demo instance runs with. endpoint is the
// base URL of the stub backend.
func Config(lang, endpoint string) config.Config {
	cfg := config.Defaults()
	cfg.Language = i18n.Normalize(lang)
	cfg.Endpoint = endpoint
	cfg.ChatDeployment = "model-router"
	cfg.APIVersion = "2024-08-01-preview"
	cfg.EmbeddingDeployment = "text-embedding-3-large"
	cfg.ImageDeployment = "gpt-image-2"
	cfg.ImageSize = "1536x1024"
	cfg.ImageQuality = "high"
	cfg.ImageFormat = "png"
	cfg.SearchProvider = "searxng"
	cfg.SearchEndpoint = searchEndpoint
	return cfg
}

// Setup prepares a demo data path: it writes the demo configuration, opens the
// database, seeds it when it is still empty and stores the section index. The
// caller owns the returned store and has to close it.
func Setup(ctx context.Context, dataDir, lang, endpoint string) (*config.Store, *storage.Store, Index, error) {
	appDataDir := filepath.Join(dataDir, "appdata")
	if err := os.MkdirAll(appDataDir, 0o750); err != nil {
		return nil, nil, Index{}, err
	}

	cfgStore := config.NewStore(filepath.Join(appDataDir, "config.json"),
		config.Keys{API: "demo-key"}, config.Overrides{ChatModels: Models})
	if _, err := cfgStore.Load(); err != nil {
		return nil, nil, Index{}, err
	}
	if err := cfgStore.Save(Config(lang, endpoint)); err != nil {
		return nil, nil, Index{}, err
	}

	dbPath := filepath.Join(dataDir, "ai-ui.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		return nil, nil, Index{}, err
	}
	if err := store.Migrate(ctx); err != nil {
		_ = store.Close()
		return nil, nil, Index{}, err
	}

	idx, err := Seed(ctx, store, dbPath, i18n.Normalize(lang))
	if err != nil {
		_ = store.Close()
		return nil, nil, Index{}, err
	}
	if err := WriteIndex(dataDir, idx); err != nil {
		_ = store.Close()
		return nil, nil, Index{}, err
	}
	return cfgStore, store, idx, nil
}
