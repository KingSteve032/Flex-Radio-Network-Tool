package utils

import (
	"net"
	"testing"
	"time"
)

func resetClientAuthTestState() {
	activeClients.Range(func(key, _ any) bool {
		activeClients.Delete(key)
		return true
	})
}

func TestNormalizeClientAuthMode(t *testing.T) {
	tests := map[string]string{
		"":               ClientAuthModeDB,
		"db":             ClientAuthModeDB,
		"netbird":        ClientAuthModeDB,
		"registered":     ClientAuthModeRegistered,
		"manual":         ClientAuthModeRegistered,
		"does-not-exist": ClientAuthModeDB,
	}

	for input, want := range tests {
		if got := NormalizeClientAuthMode(input); got != want {
			t.Fatalf("NormalizeClientAuthMode(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestRegisterClientAllowsRegisteredAuthModeWithoutDB(t *testing.T) {
	resetClientAuthTestState()
	defer resetClientAuthTestState()

	co := &ConfigOptions{ClientAuthMode: ClientAuthModeRegistered}
	addr := &net.UDPAddr{IP: net.ParseIP("100.64.0.12"), Port: 54321}

	registerClient(addr, []byte("HELLO client_ip=100.64.0.12 client_version=test"), co)

	ci, ok := getClientInfo("100.64.0.12")
	if !ok {
		t.Fatal("expected registered auth mode to accept HELLO without DB lookup")
	}
	if ci.Addr == nil || ci.Addr.Port != 54321 {
		t.Fatalf("unexpected registered client address: %+v", ci.Addr)
	}
}

func TestRegisteredDiscoveryTargetsUseOnlyLiveRegistrations(t *testing.T) {
	resetClientAuthTestState()
	defer resetClientAuthTestState()

	now := time.Now()
	activeClients.Store("100.64.0.12", &ClientInfo{
		Addr:         &net.UDPAddr{IP: net.ParseIP("100.64.0.12"), Port: 1111},
		IP:           "100.64.0.12",
		RegisteredAt: now.Add(-time.Second),
		LastSeen:     now,
	})
	activeClients.Store("100.64.0.13", &ClientInfo{
		Addr:         &net.UDPAddr{IP: net.ParseIP("100.64.0.13"), Port: 2222},
		IP:           "100.64.0.13",
		RegisteredAt: now.Add(-10 * time.Minute),
		LastSeen:     now.Add(-10 * time.Minute),
	})

	targets := registeredDiscoveryTargets(ConfigOptions{ClientAuthMode: ClientAuthModeRegistered})
	if len(targets) != 1 {
		t.Fatalf("expected one live registered target, got %d", len(targets))
	}
	if targets[0].IP != "100.64.0.12" {
		t.Fatalf("unexpected discovery target: %+v", targets[0])
	}
}
