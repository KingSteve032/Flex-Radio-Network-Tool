package utils

import (
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
