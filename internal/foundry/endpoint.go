package foundry

import (
	"errors"
	"net/url"
	"sort"
	"strings"
	"unicode"
)

func ValidateResourceID(raw string) error {
	parts := strings.Split(strings.TrimSuffix(raw, "/"), "/")
	if len(parts) != 9 || parts[0] != "" ||
		!strings.EqualFold(parts[1], "subscriptions") || !validSubscriptionID(parts[2]) ||
		!strings.EqualFold(parts[3], "resourceGroups") || !validSegment(parts[4]) ||
		!strings.EqualFold(parts[5], "providers") || !strings.EqualFold(parts[6], "Microsoft.CognitiveServices") ||
		!strings.EqualFold(parts[7], "accounts") || !validAccountName(parts[8]) {
		return errors.New("azure resource ID must identify a Microsoft.CognitiveServices account in a subscription and resource group")
	}
	return nil
}

func validSubscriptionID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func validSegment(value string) bool {
	if value == "" || len(value) > 256 || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune("/\\%?#:", r) {
			return false
		}
	}
	return true
}

func validAccountName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if !asciiAlphanumeric(r) && r != '-' {
			return false
		}
	}
	return true
}

func asciiAlphanumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// IsV1Endpoint recognizes explicit v1 paths, including HTTP URLs used by local
// manual-mode backends. Identity-backed endpoint selection separately requires HTTPS.
func IsV1Endpoint(raw string) bool {
	u, err := parseEndpoint(raw, true)
	return err == nil && strings.HasSuffix(u.Path, "/openai/v1")
}

func NormalizeEndpoint(raw string) (string, error) {
	u, err := parseEndpoint(raw, false)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(u.Path, "/openai/v1") {
		return u.String(), nil
	}
	if u.Path == "" {
		if _, domain := azureResourceHost(u.Hostname()); domain == "openai" || domain == "services" {
			u.Path = "/openai/v1"
			return u.String(), nil
		}
	}
	return "", errors.New("azure inference endpoint must be an OpenAI or AI Services resource root, or an explicit /openai/v1 URL")
}

func parseEndpoint(raw string, allowHTTP bool) (*url.URL, error) {
	invalid := errors.New("azure inference endpoint must be an absolute HTTPS URL without credentials, query, fragment, or ambiguous path segments")
	if raw != strings.TrimSpace(raw) || len(raw) > 4096 {
		return nil, invalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		(u.Scheme != "https" && (!allowHTTP || u.Scheme != "http")) ||
		strings.Contains(u.EscapedPath(), "%") {
		return nil, invalid
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return nil, invalid
		}
	}
	for strings.Contains(u.Path, "//") {
		u.Path = strings.ReplaceAll(u.Path, "//", "/")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

func azureResourceHost(host string) (name, domain string) {
	host = strings.ToLower(host)
	for _, entry := range []struct{ suffix, domain string }{
		{".openai.azure.com", "openai"},
		{".services.ai.azure.com", "services"},
		{".cognitiveservices.azure.com", "cognitive"},
	} {
		if !strings.HasSuffix(host, entry.suffix) {
			continue
		}
		name = strings.TrimSuffix(strings.TrimSuffix(host, entry.suffix), ".privatelink")
		if validAccountName(name) {
			return name, entry.domain
		}
	}
	return "", ""
}

type accountProperties struct {
	Endpoint            string            `json:"endpoint"`
	Endpoints           map[string]string `json:"endpoints"`
	CustomSubDomainName string            `json:"customSubDomainName"`
}

func selectEndpoint(properties accountProperties, override string) (string, error) {
	if override != "" {
		endpoint, err := NormalizeEndpoint(override)
		if err != nil {
			return "", err
		}
		target, _ := url.Parse(endpoint) // NormalizeEndpoint already validated the URL.
		if !matchesAccountEndpoint(target, properties) {
			return "", errors.New("azure endpoint override cannot be verified against the configured account's endpoint metadata")
		}
		return endpoint, nil
	}

	// This named endpoint is documented by example, not as a guaranteed key.
	// Accept it only when the URL itself identifies a compatible API.
	const legacyEndpoint = "Azure OpenAI Legacy API - Latest moniker"
	for _, raw := range []string{properties.Endpoints[legacyEndpoint], properties.Endpoint} {
		if endpoint, err := NormalizeEndpoint(raw); err == nil {
			return endpoint, nil
		}
	}
	candidates := make(map[string]bool)
	for _, raw := range properties.Endpoints {
		if endpoint, err := NormalizeEndpoint(raw); err == nil {
			candidates[endpoint] = true
		}
	}
	if len(candidates) == 1 {
		for candidate := range candidates {
			return candidate, nil
		}
	}
	if len(candidates) > 1 {
		return "", errors.New("azure account advertises multiple compatible inference endpoints; set an explicit same-resource Azure endpoint")
	}
	return "", errors.New("azure account metadata does not provide a compatible OpenAI v1 endpoint; set an explicit same-resource Azure endpoint")
}

func matchesAccountEndpoint(target *url.URL, properties accountProperties) bool {
	targetName, _ := azureResourceHost(target.Hostname())
	normalPort := func(u *url.URL) string {
		if u.Port() == "" {
			return "443"
		}
		return u.Port()
	}
	metadata := []string{properties.Endpoint}
	keys := make([]string, 0, len(properties.Endpoints))
	for key := range properties.Endpoints {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		metadata = append(metadata, properties.Endpoints[key])
	}
	for _, raw := range metadata {
		other, err := parseEndpoint(raw, false)
		if err != nil {
			continue
		}
		if strings.EqualFold(target.Hostname(), other.Hostname()) && normalPort(target) == normalPort(other) {
			if targetName != "" && target.Path == "/openai/v1" {
				return true
			}
			if targetName == "" {
				expectedPath := other.Path
				if !strings.HasSuffix(expectedPath, "/openai/v1") {
					expectedPath += "/openai/v1"
				}
				if target.Path == expectedPath {
					return true
				}
			}
			continue
		}
		otherName, _ := azureResourceHost(other.Hostname())
		if targetName != "" && targetName == otherName && target.Path == "/openai/v1" &&
			normalPort(target) == "443" && normalPort(other) == "443" {
			return true
		}
	}
	return targetName != "" && target.Path == "/openai/v1" && normalPort(target) == "443" &&
		strings.EqualFold(targetName, properties.CustomSubDomainName)
}
