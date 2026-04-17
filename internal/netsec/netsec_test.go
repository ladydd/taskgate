package netsec

import (
	"context"
	"net"
	"testing"
)

type mockResolver struct {
	addrs []net.IPAddr
	err   error
}

func (m mockResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return m.addrs, m.err
}

func TestResolvePublicIPsRejectsLocalhost(t *testing.T) {
	t.Parallel()
	_, err := ResolvePublicIPs(context.Background(), "localhost", nil)
	if err == nil {
		t.Fatal("expected error for localhost")
	}
}

func TestResolvePublicIPsRejectsPrivateIP(t *testing.T) {
	t.Parallel()
	_, err := ResolvePublicIPs(context.Background(), "192.168.1.1", nil)
	if err == nil {
		t.Fatal("expected error for private IP")
	}
}

func TestResolvePublicIPsRejectsLoopback(t *testing.T) {
	t.Parallel()
	_, err := ResolvePublicIPs(context.Background(), "127.0.0.1", nil)
	if err == nil {
		t.Fatal("expected error for loopback IP")
	}
}

func TestResolvePublicIPsAcceptsPublicIP(t *testing.T) {
	t.Parallel()
	ips, err := ResolvePublicIPs(context.Background(), "8.8.8.8", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ips) != 1 {
		t.Fatalf("expected 1 IP, got %d", len(ips))
	}
}

func TestResolvePublicIPsRejectsDNSToPrivate(t *testing.T) {
	t.Parallel()
	resolver := mockResolver{
		addrs: []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}},
	}
	_, err := ResolvePublicIPs(context.Background(), "evil.example.com", resolver)
	if err == nil {
		t.Fatal("expected error for DNS resolving to private IP")
	}
}

func TestNormalizeHostname(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input, want string
	}{
		{"LOCALHOST.", "localhost"},
		{"  Example.COM.  ", "example.com"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := NormalizeHostname(tt.input); got != tt.want {
			t.Errorf("NormalizeHostname(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
