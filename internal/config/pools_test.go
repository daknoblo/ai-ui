package config

import (
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

func replicaStore(t *testing.T, overrides Overrides) (*Store, foundry.Snapshot, foundry.Snapshot) {
	t.Helper()
	s := newFoundryStore(t, overrides)
	a, b := testCatalog(), testCatalog()
	b.ResourceID += "-poland"
	b.Endpoint = "https://poland.services.ai.azure.com/openai/v1"
	b.Location = "polandcentral"
	a.Location = "swedencentral"
	a.Accounts = []foundry.Snapshot{b}
	if err := s.SetCatalog(a); err != nil {
		t.Fatal(err)
	}
	return s, a, b
}

func TestPoolRoundRobinPersistenceAndSnapshot(t *testing.T) {
	s, a, b := replicaStore(t, Overrides{})
	cfg := s.Get()
	ak, bk := a.Key(a.Deployments[0]), b.Key(b.Deployments[0])
	cfg.EnabledDeployments = map[foundry.Operation][]string{foundry.Chat: {ak, bk, ak}}
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Get().EnabledDeployments[foundry.Chat], []string{ak, bk}) {
		t.Fatal("duplicate selections were persisted")
	}
	for _, endpoint := range []string{a.Endpoint, b.Endpoint, a.Endpoint} {
		bound, deployment, err := s.SelectRoute(foundry.Chat, "")
		if err != nil || bound.Get().Endpoint != endpoint || deployment.Name != "my-chat" {
			t.Fatalf("incorrect cycle: %v, %v", bound, err)
		}
		req, err := http.NewRequest(http.MethodPost, endpoint+"/chat/completions", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := bound.Authorize(req, foundry.Chat, deployment.Name); err != nil {
			t.Fatal(err)
		}
		wrong, err := http.NewRequest(http.MethodPost, "https://untrusted.example/openai/v1/chat/completions", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := bound.Authorize(wrong, foundry.Chat, deployment.Name); err == nil || wrong.Header.Get("Authorization") != "" {
			t.Fatal("credential escaped its immutable route")
		}
	}
	bound, _, err := s.SelectRoute(foundry.Chat, bk)
	if err != nil {
		t.Fatal(err)
	}
	cfg = s.Get()
	cfg.EnabledDeployments[foundry.Chat] = []string{}
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if s.IsConfigured() {
		t.Fatal("empty chat pool must disable chat")
	}
	if _, _, err := s.SelectRoute(foundry.Chat, ""); err == nil {
		t.Fatal("empty pool silently repopulated")
	}
	if bound.Get().Endpoint != b.Endpoint {
		t.Fatal("in-flight snapshot changed")
	}
	if err := s.SetCatalog(a); err != nil {
		t.Fatal(err)
	}
	if s.IsConfigured() {
		t.Fatal("refresh enabled a disabled pool")
	}
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if s.Get().EnabledDeployments == nil || len(s.EnabledPools()[foundry.Chat]) != 0 {
		t.Fatal("disabled selections did not survive reload")
	}
	cfg = s.Get()
	cfg.EnabledDeployments = map[foundry.Operation][]string{}
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if s.Get().EnabledDeployments == nil || s.IsConfigured() {
		t.Fatal("an empty persisted pool map re-enabled legacy defaults")
	}
}

func TestPoolConcurrentSelectionAndConfiguration(t *testing.T) {
	s, a, b := replicaStore(t, Overrides{})
	cfg := s.Get()
	cfg.EnabledDeployments = map[foundry.Operation][]string{
		foundry.Chat: {a.Key(a.Deployments[0]), b.Key(b.Deployments[0])},
	}
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		wg.Go(func() {
			bound, _, err := s.SelectRoute(foundry.Chat, "")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			counts[bound.Get().Endpoint]++
			mu.Unlock()
		})
	}
	wg.Go(func() {
		for i := 0; i < 20; i++ {
			if err := s.SetCatalog(a); err != nil {
				t.Error(err)
			}
			if err := s.Save(s.Get()); err != nil {
				t.Error(err)
			}
		}
	})
	wg.Wait()
	if counts[a.Endpoint] != 100 || counts[b.Endpoint] != 100 {
		t.Fatalf("concurrent cycle lost turns: %v", counts)
	}
}

func TestPoolsValidateVectorSpaceAndOperationAtomically(t *testing.T) {
	s, a, b := replicaStore(t, Overrides{})
	cfg := s.Get()
	cfg.EnabledDeployments = map[foundry.Operation][]string{
		foundry.Embeddings: {a.Key(a.Deployments[1]), b.Key(b.Deployments[1])},
	}
	if err := s.Save(cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(*foundry.Deployment){
		func(d *foundry.Deployment) { d.ModelVersion = "2" },
		func(d *foundry.Deployment) { d.ModelName = "text-embedding-3-large" },
		func(d *foundry.Deployment) { d.EmbeddingDimensions = 128 },
		func(d *foundry.Deployment) { d.ModelVersion = "" },
	} {
		b.Deployments = slices.Clone(a.Deployments)
		mutation(&b.Deployments[1])
		a.Accounts = []foundry.Snapshot{b}
		if err := s.SetCatalog(a); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(cfg); !errors.Is(err, ErrEmbeddingPool) {
			t.Fatalf("incompatible vectors accepted: %v", err)
		}
		after, err := os.ReadFile(s.path)
		if err != nil || string(before) != string(after) {
			t.Fatal("rejected selection mutated settings")
		}
	}
	cfg.EnabledDeployments = map[foundry.Operation][]string{foundry.Chat: {a.Key(a.Deployments[1])}}
	if err := s.Save(cfg); err == nil {
		t.Fatal("embedding deployment accepted into chat pool")
	}
	cfg.EnabledDeployments[foundry.Chat] = []string{"/subscriptions/unknown"}
	if err := s.Save(cfg); err == nil {
		t.Fatal("unknown resource accepted")
	}
}

func TestLegacyAndEnvironmentRemainAnchored(t *testing.T) {
	for _, overrides := range []Overrides{{}, {ChatDeployment: "my-chat"}, {Endpoint: testCatalog().Endpoint}} {
		s, a, b := replicaStore(t, overrides)
		cfg := s.Get()
		cfg.ChatDeployment = "my-chat"
		if err := s.Save(cfg); err != nil {
			t.Fatal(err)
		}
		for _, pin := range []string{"my-chat", ""} {
			bound, _, err := s.SelectRoute(foundry.Chat, pin)
			if err != nil || bound.Get().Endpoint != a.Endpoint {
				t.Fatalf("legacy selection switched resources: %v", err)
			}
		}
		cfg = s.Get()
		cfg.EnabledDeployments = map[foundry.Operation][]string{foundry.Chat: {b.Key(b.Deployments[0])}}
		err := s.Save(cfg)
		if overrides.Endpoint != "" {
			if err == nil {
				t.Fatal("environment endpoint lock was ignored")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if overrides.ChatDeployment != "" {
			bound, _, err := s.SelectRoute(foundry.Chat, "")
			if err != nil || bound.Get().Endpoint != a.Endpoint {
				t.Fatal("environment deployment lock was ignored")
			}
		}
		if !strings.HasSuffix(s.EnabledPools()[foundry.Chat][0], "/deployments/my-chat") {
			t.Fatal("selection lost resource-qualified identity")
		}
	}
}
