package netsec

import (
	"context"
	"fmt"
	"net"
	"strings"
)

// Resolver resolves hostnames to IP addresses.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ResolvePublicIPs resolves a hostname and rejects loopback, private, link-local,
// and unspecified addresses. Hostname inputs are normalized before resolution.
func ResolvePublicIPs(ctx context.Context, hostname string, resolver Resolver) ([]net.IP, error) {
	normalized := NormalizeHostname(hostname)
	if normalized == "" {
		return nil, fmt.Errorf("URL is missing a hostname")
	}

	if isLocalHostname(normalized) {
		return nil, fmt.Errorf("access to local addresses is not allowed")
	}

	if ip := net.ParseIP(normalized); ip != nil {
		if IsBlockedIP(ip) {
			return nil, fmt.Errorf("access to private/internal addresses is not allowed")
		}
		return []net.IP{ip}, nil
	}

	if resolver == nil {
		resolver = net.DefaultResolver
	}

	resolvedAddrs, err := resolver.LookupIPAddr(ctx, normalized)
	if err != nil {
		return nil, fmt.Errorf("DNS resolution failed: %s", normalized)
	}
	if len(resolvedAddrs) == 0 {
		return nil, fmt.Errorf("DNS resolution returned no results: %s", normalized)
	}

	ips := make([]net.IP, 0, len(resolvedAddrs))
	for _, addr := range resolvedAddrs {
		ip := addr.IP
		if ip == nil {
			continue
		}
		if IsBlockedIP(ip) {
			return nil, fmt.Errorf("hostname %s resolved to private address %s, access denied", normalized, ip.String())
		}
		ips = append(ips, ip)
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("DNS resolution returned no results: %s", normalized)
	}

	return ips, nil
}

// NormalizeHostname lowercases hostnames and trims any trailing dots so
// localhost-style aliases such as "LOCALHOST." are handled consistently.
func NormalizeHostname(hostname string) string {
	normalized := strings.TrimSpace(strings.ToLower(hostname))
	return strings.TrimRight(normalized, ".")
}

// IsBlockedIP returns true if the IP is loopback, private, link-local, or unspecified.
func IsBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

func isLocalHostname(hostname string) bool {
	return hostname == "localhost" || hostname == "0.0.0.0" || strings.HasSuffix(hostname, ".localhost")
}
