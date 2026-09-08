package config

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/daknoblo/ai-ui/internal/foundry"
)

// NormalizeEmbeddingEndpoint keeps profile identities independent of URL casing,
// default ports and trailing slashes. Credentials and query parameters never
// belong in the persisted identity.
func NormalizeEmbeddingEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("embedding endpoint must be an HTTP(S) URL without credentials, query parameters or fragments")
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	u.Host = host
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	return u.String(), nil
}

// EmbeddingKeyMismatch identifies a dedicated endpoint that cannot inherit the
// chat key. An explicitly supplied embedding key is scoped by its own setting.
func (s *Store) EmbeddingKeyMismatch() bool {
	cfg := s.Get()
	if cfg.Foundry || s.HasOwnEmbeddingAPIKey() || cfg.EmbeddingDeployment == "" {
		return false
	}
	endpoint, err := NormalizeEmbeddingEndpoint(cfg.EmbeddingHost())
	if err != nil {
		return false
	}
	chat, err := NormalizeEmbeddingEndpoint(cfg.Endpoint)
	if err != nil {
		return true
	}
	base, _ := url.Parse(endpoint) // Both URLs were validated by normalization.
	chatBase, _ := url.Parse(chat)
	return base.Scheme != chatBase.Scheme || base.Host != chatBase.Host
}

// rememberEmbeddingEndpointLocked binds only explicitly configured destinations
// to this process's immutable keys. A persisted index must never authorize a
// destination on its own, especially after a restart with different credentials.
func (s *Store) rememberEmbeddingEndpointLocked(cfg Config) {
	if cfg.Foundry || cfg.EmbeddingDeployment == "" {
		return
	}
	endpoint, err := NormalizeEmbeddingEndpoint(cfg.EmbeddingHost())
	if err != nil {
		return
	}
	if s.embeddingAPIKey == "" {
		chat, err := NormalizeEmbeddingEndpoint(cfg.Endpoint)
		if err != nil {
			return
		}
		base, _ := url.Parse(endpoint) // Normalization above validated both URLs.
		chatBase, _ := url.Parse(chat)
		if base.Scheme != chatBase.Scheme || base.Host != chatBase.Host {
			return
		}
	}
	if s.embeddingEndpoints == nil {
		s.embeddingEndpoints = make(map[string]struct{})
	}
	s.embeddingEndpoints[endpoint] = struct{}{}
}

func (s *Store) authorizeEmbeddingKey(req *http.Request, deployment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rememberEmbeddingEndpointLocked(s.effectiveLocked())
	requestEndpoint := *req.URL
	requestEndpoint.RawQuery = ""
	requestEndpoint.ForceQuery = false
	requestURL, err := NormalizeEmbeddingEndpoint(requestEndpoint.String())
	if err != nil {
		return err
	}
	for endpoint := range s.embeddingEndpoints {
		path := endpoint + "/openai/deployments/" + url.PathEscape(deployment) + "/embeddings"
		if foundry.IsV1Endpoint(endpoint) {
			path = endpoint + "/embeddings"
		}
		if requestURL == path {
			key := s.EmbeddingAPIKey()
			if key == "" {
				return fmt.Errorf("no embedding API key configured")
			}
			req.Header.Set("api-key", key)
			return nil
		}
	}
	return fmt.Errorf("embedding endpoint is not authorized for this API key; configure a matching endpoint or a dedicated embedding key and rebuild the index")
}
