package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/daknoblo/ai-ui/internal/config"
)

// Message ist eine Chat-Nachricht im OpenAI-Format.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client spricht mit dem Azure-OpenAI-kompatiblen Model-Router.
type Client struct {
	store *config.Store
	http  *http.Client
}

// New erzeugt einen neuen LLM-Client.
func New(store *config.Store) *Client {
	return &Client{
		store: store,
		http:  &http.Client{Timeout: 5 * time.Minute},
	}
}

// chatRequest ist der Request-Body für Chat Completions.
type chatRequest struct {
	Model       string    `json:"model,omitempty"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
}

// streamChunk ist ein einzelnes SSE-Delta der Chat-Completions-Antwort.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// ChatStream sendet die Nachrichten und ruft onDelta für jedes Text-Token auf.
func (c *Client) ChatStream(ctx context.Context, messages []Message, onDelta func(string) error) error {
	cfg := c.store.Get()
	if cfg.Endpoint == "" || cfg.ChatDeployment == "" || cfg.APIVersion == "" {
		return fmt.Errorf("konfiguration unvollständig: endpoint, chat-deployment und api-version erforderlich")
	}
	if !c.store.HasAPIKey() {
		return fmt.Errorf("kein API-Key gesetzt (AZURE_API_KEY)")
	}

	url := fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
		strings.TrimRight(cfg.Endpoint, "/"), cfg.ChatDeployment, cfg.APIVersion)

	body, err := json.Marshal(chatRequest{
		Model:       cfg.ChatModel,
		Messages:    messages,
		Temperature: cfg.Temperature,
		Stream:      true,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.store.APIKey())
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // unvollständige/leere Zeilen überspringen
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				if err := onDelta(ch.Delta.Content); err != nil {
					return err
				}
			}
		}
	}
	return scanner.Err()
}

// VerifyChat prüft mit einer minimalen Anfrage, ob der Chat-Endpoint erreichbar
// ist und gültig antwortet.
func (c *Client) VerifyChat(ctx context.Context) error {
	cfg := c.store.Get()
	if cfg.Endpoint == "" || cfg.ChatDeployment == "" || cfg.APIVersion == "" {
		return fmt.Errorf("endpoint, chat-deployment und api-version erforderlich")
	}
	if !c.store.HasAPIKey() {
		return fmt.Errorf("kein API-Key gesetzt (AZURE_API_KEY)")
	}

	url := fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
		strings.TrimRight(cfg.Endpoint, "/"), cfg.ChatDeployment, cfg.APIVersion)

	body, err := json.Marshal(chatRequest{
		Model:     cfg.ChatModel,
		Messages:  []Message{{Role: "user", Content: "ping"}},
		Stream:    false,
		MaxTokens: 1,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.store.APIKey())

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	return nil
}

// VerifyEmbedding prüft mit einer minimalen Anfrage, ob der Embedding-Endpoint
// erreichbar ist und ein gültiges Embedding liefert.
func (c *Client) VerifyEmbedding(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	vecs, err := c.Embed(ctx, []string{"ping"})
	if err != nil {
		return err
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return fmt.Errorf("kein gültiges embedding erhalten")
	}
	return nil
}

// embeddingRequest ist der Request-Body für die Embeddings-API.
type embeddingRequest struct {
	Input []string `json:"input"`
}

// embeddingResponse ist die Antwort der Embeddings-API.
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// Embed erzeugt Embeddings für die übergebenen Texte.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	cfg := c.store.Get()
	if cfg.EmbeddingDeployment == "" {
		return nil, fmt.Errorf("kein embedding-deployment konfiguriert")
	}
	if !c.store.HasEmbeddingAPIKey() {
		return nil, fmt.Errorf("kein API-Key gesetzt (AZURE_API_KEY bzw. AZURE_EMBEDDING_API_KEY)")
	}

	url := fmt.Sprintf("%s/openai/deployments/%s/embeddings?api-version=%s",
		strings.TrimRight(cfg.EmbeddingHost(), "/"), cfg.EmbeddingDeployment, cfg.EmbeddingVersion())

	body, err := json.Marshal(embeddingRequest{Input: inputs})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", c.store.EmbeddingAPIKey())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}

	var out embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	result := make([][]float32, len(out.Data))
	for _, d := range out.Data {
		if d.Index >= 0 && d.Index < len(result) {
			result[d.Index] = d.Embedding
		}
	}
	return result, nil
}

// readError liest eine Fehlerantwort aus und formatiert sie.
func readError(resp *http.Response) error {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	msg := strings.TrimSpace(buf.String())
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return fmt.Errorf("azure-fehler %d: %s", resp.StatusCode, msg)
}
