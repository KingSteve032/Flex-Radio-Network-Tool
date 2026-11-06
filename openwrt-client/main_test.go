package main

import (
	"net"
	"testing"
)

func TestDiscoveryRejectsUntrustedPackets(t *testing.T) {
	for _, raw := range [][]byte{
		nil,
		[]byte("HEARTBEAT"),
		[]byte("serial=FAKE ip=192.168.1.9 port=4992 discovery_protocol_version=3"),
		[]byte("HELLO client_ip=100.64.0.5"),
	} {
		if _, _, err := discoveryFields(raw); err == nil {
			t.Fatalf("accepted non-VRT packet %q", raw)
		}
	}
}

func TestServerIPv4(t *testing.T) {
	for _, address := range []string{"100.64.0.4", "10.1.2.3", "192.168.8.1"} {
		if got := net.ParseIP(address); got == nil || got.To4() == nil {
			t.Fatalf("expected IPv4 address %s", address)
		}
	}
}
