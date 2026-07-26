package websearch

import (
	"net"
	"testing"
)

// TestValidateEndpoint covers the checks applied to the user supplied SearXNG
// base URL before it is stored.
func TestValidateEndpoint(t *testing.T) {
	valid := []string{
		"",
		"https://searxng.example.com",
		"http://searxng:8080",
		"https://searxng.example.com/search",
	}
	for _, in := range valid {
		if err := ValidateEndpoint(in); err != nil {
			t.Errorf("ValidateEndpoint(%q) = %v, want nil", in, err)
		}
	}

	invalid := []string{
		"file:///etc/passwd",
		"gopher://example.com",
		"https://",
		"https://user:pass@example.com",
		"not a url at all",
	}
	for _, in := range invalid {
		if err := ValidateEndpoint(in); err == nil {
			t.Errorf("ValidateEndpoint(%q) = nil, want an error", in)
		}
	}
}

// TestIsAllowedIP documents which destinations the hardened dialer refuses.
// Private LAN ranges stay allowed on purpose because a self-hosted SearXNG
// instance usually lives there.
func TestIsAllowedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"::1",
		"0.0.0.0",
		"169.254.169.254", // cloud metadata service
		"fe80::1",
	}
	for _, ip := range blocked {
		if isAllowedIP(net.ParseIP(ip)) {
			t.Errorf("%s should be blocked", ip)
		}
	}

	allowed := []string{"10.1.2.3", "172.16.0.5", "192.168.1.10", "1.1.1.1", "2606:4700::1111"}
	for _, ip := range allowed {
		if !isAllowedIP(net.ParseIP(ip)) {
			t.Errorf("%s should be allowed", ip)
		}
	}
}
