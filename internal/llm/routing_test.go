package llm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/daknoblo/ai-ui/internal/config"
	"github.com/daknoblo/ai-ui/internal/foundry"
	"github.com/daknoblo/ai-ui/internal/storage"
)

func pooledClient(t *testing.T, handler func(string, http.ResponseWriter, *http.Request)) (*Client, *config.Store) {
	t.Helper()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler("A", w, r) }))
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler("B", w, r) }))
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	client, store := identityClient(t, a.URL)
	root := store.FoundryStatus().Catalog
	for i := range root.Deployments {
		if root.Deployments[i].Supports(foundry.Embeddings) {
			root.Deployments[i].EmbeddingDimensions = 2
		}
	}
	replica := root
	replica.ResourceID += "-poland"
	replica.Endpoint = b.URL + "/openai/v1"
	root.Accounts = []foundry.Snapshot{replica}
	if err := store.SetCatalog(root); err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	cfg.EnabledDeployments = map[foundry.Operation][]string{}
	for _, op := range config.Operations {
		name := "talk"
		switch op {
		case foundry.Images, foundry.ImageEdits:
			name = "draw"
		case foundry.Embeddings:
			name = "new-vectors"
		}
		d, ok := root.Find(name)
		if !ok {
			t.Fatal(name)
		}
		cfg.EnabledDeployments[op] = []string{root.Key(d), replica.Key(d)}
	}
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return client, store
}

func TestChatRoutesAreFrozenAcrossToolPassesAndSettings(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	client, store := pooledClient(t, func(provider string, w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || r.Header.Get("api-key") != "" {
			t.Error("route did not authorize with its identity")
		}
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "talk" {
			t.Errorf("resource ID leaked into model field: %+v, %v", body, err)
		}
		mu.Lock()
		calls = append(calls, provider)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := fmt.Fprintf(w, "data: {\"model\":\"actual-%s\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n", provider); err != nil {
			t.Error(err)
		}
	})
	opts, err := client.PrepareChat(ChatOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	cfg.Temperature = 0.7
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err := client.ChatStreamWithTools(t.Context(), opts, []Message{{Role: "user", Content: "test"}}, nil, func(string) error { return nil })
		if err != nil || result.Model != "actual-A" {
			t.Fatalf("tool pass switched route: %+v, %v", result, err)
		}
	}
	for _, expected := range []string{"actual-B", "actual-A"} {
		result, err := client.ChatStream(t.Context(), ChatOptions{}, []Message{{Role: "user", Content: "test"}}, func(string) error { return nil })
		if err != nil || result.Model != expected {
			t.Fatalf("new user turn: %+v, %v", result, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, []string{"A", "A", "B", "A"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestResponsesAndVisionUseIndependentImmutablePools(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	client, store := pooledClient(t, func(provider string, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/responses" {
			t.Errorf("expected Responses route, got %s", r.URL.Path)
		}
		mu.Lock()
		calls = append(calls, provider)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n") // Local SSE fixture.
	})
	root := store.FoundryStatus().Catalog
	root.Deployments[0].ModelName = "gpt-6-astra"
	root.Accounts[0].Deployments[0].ModelName = "gpt-6-astra"
	if err := store.SetCatalog(root); err != nil {
		t.Fatal(err)
	}
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "test", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	opts, err := client.PrepareChat(ChatOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	messages := []Message{{Role: "user", Content: "test", Images: []ImageContent{{MIME: "image/png", Data: []byte("fixture")}}}}
	for range 2 {
		result, err := client.ChatStreamWithTools(t.Context(), opts, messages, tools, func(string) error { return nil })
		if err != nil || result.Model != "talk" {
			t.Fatalf("Responses continuation lost provider: %+v, %v", result, err)
		}
	}
	if _, err := client.ChatStreamWithTools(t.Context(), ChatOptions{}, messages, tools, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Chat has a separate cycle: vision activity must not advance it.
	if _, err := client.ChatStreamWithTools(t.Context(), ChatOptions{}, []Message{{Role: "user", Content: "test"}}, tools, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, []string{"A", "A", "B", "A"}) {
		t.Fatalf("operation pools are not independent: %v", calls)
	}
}

func TestRoutingHonorsToolCapabilitiesAndEmptyVisionPool(t *testing.T) {
	client, store := pooledClient(t, func(_ string, w http.ResponseWriter, _ *http.Request) {
		t.Error("disabled capability reached inference")
		w.WriteHeader(http.StatusBadRequest)
	})
	root := store.FoundryStatus().Catalog
	root.Deployments[0].Capabilities = map[string]string{"toolCalling": "false"}
	if err := store.SetCatalog(root); err != nil {
		t.Fatal(err)
	}
	opts, err := client.PrepareChat(ChatOptions{Model: "talk"}, false)
	if err != nil || opts.SupportsTools() {
		t.Fatalf("explicit capability restriction ignored: %v", err)
	}
	if _, err := client.ChatStreamWithTools(t.Context(), opts, nil, []Tool{{Type: "function"}}, func(string) error { return nil }); err == nil {
		t.Fatal("unsupported function tools accepted")
	}
	cfg := store.Get()
	cfg.EnabledDeployments[foundry.Vision] = nil
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PrepareChat(ChatOptions{Model: "talk"}, true); err == nil {
		t.Fatal("explicit pin re-enabled an empty vision pool")
	}
}

func TestImageReplicaEditsKeepMultipartSourceAndNeverReplay(t *testing.T) {
	var calls []string
	client, _ := pooledClient(t, func(provider string, w http.ResponseWriter, r *http.Request) {
		calls = append(calls, provider)
		if strings.HasSuffix(r.URL.Path, "/edits") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = r.MultipartForm.RemoveAll() }() // Release fixture form resources.
			file, _, err := r.FormFile("image")
			if err != nil {
				t.Error(err)
				return
			}
			data, readErr := io.ReadAll(file)
			_ = file.Close() // Fixture bytes have already been read.
			if readErr != nil || string(data) != "latest-successful-image" || r.FormValue("model") != "draw" {
				t.Errorf("source/model changed: %q, %v", data, readErr)
			}
		}
		if len(calls) == 4 {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"b64_json":"bGF0ZXN0LXN1Y2Nlc3NmdWwtaW1hZ2U="}]}`) // Local fixture.
	})
	source := ImageSource{Name: "latest.png", MIME: "image/png", Data: []byte("latest-successful-image")}
	for i := 0; i < 3; i++ {
		result, err := client.EditImage(t.Context(), "refine", source, ImageOptions{})
		if err != nil || result.Model != "draw" || string(result.Data) != string(source.Data) {
			t.Fatalf("edit %d: %+v, %v", i, result, err)
		}
	}
	if _, err := client.EditImage(t.Context(), "refine", source, ImageOptions{}); err == nil {
		t.Fatal("ambiguous gateway failure was hidden")
	}
	if !slices.Equal(calls, []string{"A", "B", "A", "B"}) {
		t.Fatalf("unexpected replay or rotation: %v", calls)
	}
}

func TestEmbeddingReplicasPreserveActiveVectorSpaceAndAuthorization(t *testing.T) {
	var calls []string
	client, store := pooledClient(t, func(provider string, w http.ResponseWriter, r *http.Request) {
		calls = append(calls, provider)
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "new-vectors" {
			t.Errorf("wrong vector model: %+v, %v", body, err)
		}
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.25,0.75]}]}`) // Local fixture.
	})
	profile, err := client.ConfiguredEmbeddingProfile()
	if err != nil || profile.VectorSpace == "" || profile.Dimensions != 2 {
		t.Fatalf("replica profile = %+v, %v", profile, err)
	}
	legacy := profile
	legacy.VectorSpace = ""
	for range 3 {
		if _, err := client.EmbedProfile(t.Context(), legacy, []string{"text"}); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(calls, []string{"A", "B", "A"}) {
		t.Fatalf("embedding replicas did not rotate: %v", calls)
	}
	cfg := store.Get()
	cfg.EnabledDeployments[foundry.Embeddings] = cfg.EnabledDeployments[foundry.Embeddings][1:]
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	replica, err := client.ConfiguredEmbeddingProfile()
	if err != nil || !legacy.SameIdentity(replica) || replica.Endpoint == legacy.Endpoint {
		t.Fatalf("compatible replica requires unnecessary reindex: %+v, %v", replica, err)
	}
	if _, err := client.EmbedProfile(t.Context(), legacy, []string{"query"}); err != nil {
		t.Fatal(err)
	}
	wrong := storage.EmbeddingProfile{
		ResourceID: "/subscriptions/untrusted", Endpoint: "https://untrusted.example/openai/v1",
		Deployment: "unknown", ModelName: "unknown", ModelVersion: "1", Dimensions: 2,
	}
	if _, err := client.EmbedProfile(t.Context(), wrong, []string{"query"}); err == nil {
		t.Fatal("database profile authorized an unknown destination")
	}
	cfg = store.Get()
	cfg.EnabledDeployments[foundry.Embeddings] = nil
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := client.EmbedProfile(t.Context(), legacy, []string{"query"}); err == nil {
		t.Fatal("disabled embedding pool still used the old index")
	}
}
