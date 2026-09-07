package demo

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/rag"
	"github.com/daknoblo/ai-ui/internal/storage"
)

const demoResourceID = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/demo/providers/Microsoft.CognitiveServices/accounts/local-demo"

// SetupFoundry prepares the identity-backed inventory demo using only a local
// stub. Setup remains the manual/API-key demo for existing callers.
func SetupFoundry(ctx context.Context, dataDir, lang, endpoint string) (*config.Store, *storage.Store, Index, error) {
	source, err := newDemoFoundrySource(endpoint)
	if err != nil {
		return nil, nil, Index{}, err
	}
	snapshot, err := source.Refresh(ctx, "")
	if err != nil {
		return nil, nil, Index{}, err
	}
	cfg := Config(lang, snapshot.Endpoint)
	cfg.ChatDeployment = "chat-primary"
	cfg.ChatModel = "chat-primary"
	cfg.EmbeddingDeployment = "docs-primary"
	cfg.ImageDeployment = "canvas"
	cfg.VisionDeployment = "chat-primary"
	cfg.APIVersion = ""

	cfgStore, store, idx, err := setup(ctx, dataDir, lang, cfg, config.Keys{}, config.Overrides{})
	if err != nil {
		return nil, nil, Index{}, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = store.Close() // Preserve the setup error if cleanup also fails.
		}
	}()
	cfgStore.ConfigureFoundry(demoResourceID, source, nil)
	if err := cfgStore.SetCatalog(snapshot); err != nil {
		return nil, nil, Index{}, err
	}
	if err := cfgStore.ValidateRoleSelections(cfg); err != nil {
		return nil, nil, Index{}, err
	}
	if err := store.SaveCatalog(ctx, snapshot); err != nil {
		return nil, nil, Index{}, err
	}
	if err := remapFoundryChats(ctx, store, idx, snapshot); err != nil {
		return nil, nil, Index{}, err
	}

	deployment, ok := snapshot.Find(cfg.EmbeddingDeployment)
	if !ok {
		return nil, nil, Index{}, fmt.Errorf("demo embedding deployment is missing")
	}
	target := storage.EmbeddingProfile{
		ResourceID: strings.ToLower(demoResourceID), Endpoint: snapshot.Endpoint,
		Deployment: deployment.Name, ModelName: deployment.ModelName,
		ModelVersion: deployment.ModelVersion, Dimensions: embeddingDim,
	}
	active, known, err := store.ActiveEmbeddingProfile(ctx)
	if err != nil {
		return nil, nil, Index{}, err
	}
	if !known || !active.SameIdentity(target) || active.Dimensions != target.Dimensions {
		job, err := store.StartReindex(ctx, target)
		if err != nil {
			return nil, nil, Index{}, err
		}
		// Even the seeded vectors must be rebuilt, never relabeled as a new model.
		if err := rag.Reindex(ctx, store, job, func(ctx context.Context, _ storage.EmbeddingProfile, texts []string) ([][]float32, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			vectors := make([][]float32, len(texts))
			for i, text := range texts {
				vectors[i] = Embedding(text)
			}
			return vectors, nil
		}); err != nil {
			return nil, nil, Index{}, err
		}
	}
	idx.Foundry = true
	if err := WriteIndex(dataDir, idx); err != nil {
		return nil, nil, Index{}, err
	}
	complete = true
	return cfgStore, store, idx, nil
}

func remapFoundryChats(ctx context.Context, store *storage.Store, idx Index, snapshot foundry.Snapshot) error {
	seeded := make(map[int64]bool, len(idx.Chats))
	for _, id := range idx.Chats {
		seeded[id] = true
	}
	chats, err := store.ListChats(ctx)
	if err != nil {
		return err
	}
	for _, chat := range chats {
		if !seeded[chat.ID] {
			continue
		}
		if _, ok := snapshot.Find(chat.Model); !ok {
			model := "chat-primary"
			if chat.Model == "o4-mini" {
				model = "chat-quick"
			}
			if err := store.UpdateChatModel(ctx, chat.ID, model); err != nil {
				return err
			}
		}
		if chat.Mode == storage.ChatModeImage {
			if _, ok := snapshot.Find(chat.ImageModel); !ok {
				if err := store.UpdateChatImageModel(ctx, chat.ID, "canvas"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type demoFoundrySource struct {
	endpoint string
	host     string
}

func newDemoFoundrySource(endpoint string) (*demoFoundrySource, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid demo backend URL: %w", err)
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || ip == nil || !ip.IsLoopback() || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("demo Foundry backend must be an HTTP loopback base URL")
	}
	u.Path = "/openai/v1"
	return &demoFoundrySource{endpoint: u.String(), host: u.Host}, nil
}

func (s *demoFoundrySource) Refresh(ctx context.Context, override string) (foundry.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return foundry.Snapshot{}, err
	}
	if override != "" && strings.TrimRight(override, "/") != s.endpoint {
		return foundry.Snapshot{}, fmt.Errorf("demo Foundry endpoint cannot be overridden")
	}
	deployment := func(name, model, version string) foundry.Deployment {
		return foundry.Deployment{
			ID: demoResourceID + "/deployments/" + name, Name: name,
			ModelName: model, ModelVersion: version, ModelFormat: "OpenAI",
			ProvisioningState: "Succeeded", SKU: "GlobalStandard",
		}
	}
	deployments := []foundry.Deployment{
		deployment("chat-primary", "gpt-4o", "2024-11-20"),
		deployment("chat-quick", "gpt-4o-mini", "2024-07-18"),
		deployment("docs-primary", "text-embedding-3-large", "1"),
		deployment("docs-next", "text-embedding-3-small", "1"),
		deployment("canvas", "gpt-image-1", "2025-04-15"),
		deployment("native-chat", "claude-sonnet-4-5", "1"),
		deployment("nightly-batch", "gpt-4o", "2024-11-20"),
	}
	deployments[5].ModelFormat = "Anthropic"
	deployments[6].SKU = "GlobalBatch"
	return foundry.Snapshot{
		ResourceID: demoResourceID, Endpoint: s.endpoint,
		Deployments: deployments, RefreshedAt: time.Now().UTC(),
	}, nil
}

func (s *demoFoundrySource) Authorize(req *http.Request) error {
	if req == nil || req.URL == nil || req.URL.Scheme != "http" || req.URL.Host != s.host ||
		!strings.HasPrefix(req.URL.Path, "/openai/v1/") {
		return fmt.Errorf("demo identity only authorizes its local inference stub")
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Del("api-key")
	req.Header.Set("Authorization", "Bearer demo-identity")
	return nil
}
