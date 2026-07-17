package utils

import (
	"net"
	"testing"
	"time"
)

func resetProxySessionTestState() {
	activeProxySessions.Range(func(key, _ any) bool {
		activeProxySessions.Delete(key)
		return true
	})
	selectedSerialByClient.Range(func(key, _ any) bool {
		selectedSerialByClient.Delete(key)
		return true
	})
	pendingProxyLANSources = map[string]int{}
}

func TestCloseConflictingSessionsAllowsSameRadioWhenMultiProxy(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	closed := false
	activeProxySessions.Store(uint64(1), &proxySession{
		ID:       1,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4991,
		LastSeen: time.Now(),
		closeNow: func() { closed = true },
	})

	closeConflictingSessionsForClient("100.85.69.106", "1121-1104-6700-2912", "10.2.0.12", true)

	if closed {
		t.Fatal("multi-proxy mode closed an existing same-radio session")
	}
}

func TestCloseConflictingSessionsClosesClientSessionsWhenSingleProxy(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	closed := false
	activeProxySessions.Store(uint64(1), &proxySession{
		ID:       1,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4991,
		LastSeen: time.Now(),
		closeNow: func() { closed = true },
	})

	closeConflictingSessionsForClient("100.85.69.106", "1121-1104-6700-2912", "10.2.0.12", false)

	if !closed {
		t.Fatal("single-proxy mode did not close an existing client session")
	}
}

func TestGetVitaProxyTargetsMatchesRadioDestinationPort(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	SetClientSelectedProxySerial("100.85.69.106", "1121-1104-6700-2912")
	now := time.Now()
	activeProxySessions.Store(uint64(1), &proxySession{
		ID:       1,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4991,
		LastSeen: now,
	})
	activeProxySessions.Store(uint64(2), &proxySession{
		ID:       2,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4993,
		LastSeen: now.Add(time.Second),
	})

	targets := GetVitaProxyTargets("10.2.0.12", 4993)

	if len(targets) != 1 {
		t.Fatalf("expected one exact VITA target, got %d", len(targets))
	}
	if targets[0].ClientIP != "100.85.69.106" || targets[0].Port != 4993 {
		t.Fatalf("unexpected VITA target: %+v", targets[0])
	}
}

func TestGetVitaProxyTargetsDedupesExactClientPort(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	now := time.Now()
	activeProxySessions.Store(uint64(1), &proxySession{
		ID:       1,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4991,
		LastSeen: now,
	})
	activeProxySessions.Store(uint64(2), &proxySession{
		ID:       2,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4991,
		LastSeen: now.Add(time.Second),
	})

	targets := GetVitaProxyTargets("10.2.0.12", 4991)

	if len(targets) != 1 {
		t.Fatalf("expected one deduped exact VITA target, got %d", len(targets))
	}
	if targets[0].ClientIP != "100.85.69.106" || targets[0].Port != 4991 {
		t.Fatalf("unexpected VITA target: %+v", targets[0])
	}
}

func TestGetVitaProxyTargetsIgnoresSelectedSerialMismatchForActiveSession(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	SetClientSelectedProxySerial("100.85.69.106", "different-radio")
	activeProxySessions.Store(uint64(1), &proxySession{
		ID:       1,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4991,
		LastSeen: time.Now(),
	})

	targets := GetVitaProxyTargets("10.2.0.12", 4991)

	if len(targets) != 1 {
		t.Fatalf("expected active session target despite selected serial mismatch, got %d", len(targets))
	}
}

func TestGetVitaProxyTargetsFallsBackToNewestSession(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	activeProxySessions.Store(uint64(1), &proxySession{
		ID:       1,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4991,
		LastSeen: time.Now().Add(-time.Second),
	})
	activeProxySessions.Store(uint64(2), &proxySession{
		ID:       2,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4993,
		LastSeen: time.Now(),
	})

	targets := GetVitaProxyTargets("10.2.0.12", 5999)

	if len(targets) != 1 {
		t.Fatalf("expected one fallback VITA target, got %d", len(targets))
	}
	if targets[0].Port != 4993 {
		t.Fatalf("expected newest session fallback port 4993, got %d", targets[0].Port)
	}
}

func TestGetVitaProxyTargetsFallbackReturnsNewestPerClient(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	now := time.Now()
	activeProxySessions.Store(uint64(1), &proxySession{
		ID:       1,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4991,
		LastSeen: now.Add(-2 * time.Second),
	})
	activeProxySessions.Store(uint64(2), &proxySession{
		ID:       2,
		ClientIP: "100.85.69.106",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4993,
		LastSeen: now.Add(-time.Second),
	})
	activeProxySessions.Store(uint64(3), &proxySession{
		ID:       3,
		ClientIP: "100.85.69.107",
		Serial:   "1121-1104-6700-2912",
		RadioIP:  "10.2.0.12",
		UDPPort:  4994,
		LastSeen: now,
	})

	targets := GetVitaProxyTargets("10.2.0.12", 5999)

	if len(targets) != 2 {
		t.Fatalf("expected newest fallback target for each client, got %d", len(targets))
	}

	portsByClient := map[string]int{}
	for _, target := range targets {
		portsByClient[target.ClientIP] = target.Port
	}
	if portsByClient["100.85.69.106"] != 4993 {
		t.Fatalf("expected client 100.85.69.106 fallback port 4993, got %d", portsByClient["100.85.69.106"])
	}
	if portsByClient["100.85.69.107"] != 4994 {
		t.Fatalf("expected client 100.85.69.107 fallback port 4994, got %d", portsByClient["100.85.69.107"])
	}
}

func TestGetVitaProxyTargetsForDestinationMatchesSourceLANIP(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	now := time.Now()
	activeProxySessions.Store(uint64(1), &proxySession{
		ID:          1,
		ClientIP:    "100.85.69.106",
		Serial:      "1121-1104-6700-2912",
		RadioIP:     "10.2.0.12",
		SourceLANIP: "10.2.0.4",
		UDPPort:     4991,
		LastSeen:    now,
	})
	activeProxySessions.Store(uint64(2), &proxySession{
		ID:          2,
		ClientIP:    "100.85.69.107",
		Serial:      "1121-1104-6700-2912",
		RadioIP:     "10.2.0.12",
		SourceLANIP: "10.2.0.5",
		UDPPort:     4991,
		LastSeen:    now,
	})

	targets := GetVitaProxyTargetsForDestination("10.2.0.12", "10.2.0.5", 4991)

	if len(targets) != 1 {
		t.Fatalf("expected one target for destination 10.2.0.5, got %d", len(targets))
	}
	if targets[0].ClientIP != "100.85.69.107" {
		t.Fatalf("expected 100.85.69.107, got %+v", targets[0])
	}
}

func TestChooseProxyLANSourceIPForSessionUsesLeastUsedSource(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	co := ConfigOptions{
		ProxyLANSourceIPs: []net.IP{
			net.ParseIP("10.2.0.4"),
			net.ParseIP("10.2.0.5"),
		},
	}

	first := chooseProxyLANSourceIPForSession("100.85.69.106", "1121-1104-6700-2912", co)
	second := chooseProxyLANSourceIPForSession("100.85.69.106", "1121-1104-6700-2912", co)

	if first == "" || second == "" {
		t.Fatalf("expected source IP assignments, got first=%q second=%q", first, second)
	}
	if first == second {
		t.Fatalf("expected concurrent sessions to spread across source IPs, both got %q", first)
	}
}

func TestChooseProxyLANSourceIPDefaultsToSendInterfaceWhenPoolEmpty(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	co := ConfigOptions{
		SendNetworkInterface: NetInteface{IPAddress: net.ParseIP("10.2.0.4")},
	}

	got := chooseProxyLANSourceIPForSession("100.85.69.106", "1121-1104-6700-2912", co)
	if got != "10.2.0.4" {
		t.Fatalf("expected default source LAN IP 10.2.0.4, got %q", got)
	}
}

func TestFindProxyLANSourceIPForClientSerialUDPPortPrefersExactPort(t *testing.T) {
	resetProxySessionTestState()
	defer resetProxySessionTestState()

	activeProxySessions.Store(uint64(1), &proxySession{
		ID:          1,
		ClientIP:    "100.85.69.106",
		Serial:      "1121-1104-6700-2912",
		RadioIP:     "10.2.0.12",
		SourceLANIP: "10.2.0.4",
		UDPPort:     4991,
		LastSeen:    time.Now().Add(-time.Second),
	})
	activeProxySessions.Store(uint64(2), &proxySession{
		ID:          2,
		ClientIP:    "100.85.69.106",
		Serial:      "1121-1104-6700-2912",
		RadioIP:     "10.2.0.12",
		SourceLANIP: "10.2.0.5",
		UDPPort:     4993,
		LastSeen:    time.Now(),
	})

	got := findProxyLANSourceIPForClientSerialUDPPort("100.85.69.106", "1121-1104-6700-2912", 4991)
	if got != "10.2.0.4" {
		t.Fatalf("expected exact UDP port source LAN IP 10.2.0.4, got %q", got)
	}

	got = findProxyLANSourceIPForClientSerialUDPPort("100.85.69.106", "1121-1104-6700-2912", 0)
	if got != "10.2.0.5" {
		t.Fatalf("expected newest source LAN IP 10.2.0.5, got %q", got)
	}
}

func TestParseVitaTXPayload(t *testing.T) {
	payload := append([]byte(vitaTxPacketMagicV2), byte(len("Radio-1")))
	payload = append(payload, []byte("Radio-1")...)
	payload = append(payload, 0x13, 0x7f)
	payload = append(payload, []byte{0x01, 0x02, 0x03}...)

	serial, srcUDPPort, data, ok := parseVitaTXPayload(payload)
	if !ok {
		t.Fatal("expected VITA TX payload to parse")
	}
	if serial != "radio-1" {
		t.Fatalf("expected normalized serial radio-1, got %q", serial)
	}
	if srcUDPPort != 4991 {
		t.Fatalf("expected source UDP port 4991, got %d", srcUDPPort)
	}
	if string(data) != string([]byte{0x01, 0x02, 0x03}) {
		t.Fatalf("unexpected data: %v", data)
	}
}
