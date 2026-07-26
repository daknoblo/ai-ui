package websearch

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ValidateEndpoint checks a user supplied search endpoint before it is stored.
// Only absolute http(s) URLs with a host are accepted, which rules out schemes
// such as file://, gopher:// or data: that could otherwise be abused to make
// the server read local resources.
func ValidateEndpoint(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil // empty simply disables the provider
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("unsupported scheme %q, use http or https", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	if u.User != nil {
		return fmt.Errorf("credentials in the URL are not supported")
	}
	return nil
}

// newSafeTransport builds the HTTP transport used for all search providers.
//
// Because the SearXNG base URL is user supplied, the dialer inspects the
// resolved address of every connection. This also covers DNS rebinding, which a
// pure URL check cannot catch. Private RFC1918 ranges stay reachable on purpose:
// a self-hosted SearXNG instance usually lives on the same private network.
// Loopback and link-local addresses are rejected because they would only ever
// target the container itself or the cloud metadata service (169.254.169.254).
func newSafeTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("could not parse address %q", host)
			}
			if !isAllowedIP(ip) {
				return fmt.Errorf("connections to %s are blocked", ip)
			}
			return nil
		},
	}

	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dialer.DialContext
	t.MaxIdleConns = 16
	t.MaxIdleConnsPerHost = 4
	t.IdleConnTimeout = 60 * time.Second
	t.ForceAttemptHTTP2 = true
	return t
}

// isAllowedIP reports whether an outbound connection to ip is permitted.
// The net.IP predicates already handle IPv4-mapped IPv6 addresses.
func isAllowedIP(ip net.IP) bool {
	return !ip.IsLoopback() &&
		!ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsInterfaceLocalMulticast()
}
