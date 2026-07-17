package flexclient

import (
	"net"
	"strconv"
	"testing"
	"time"
)

func TestMaybeSendDirectRadioPunchSendsKeepalive(t *testing.T) {
	resetDirectPunchConns()
	t.Cleanup(resetDirectPunchConns)

	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP failed: %v", err)
	}
	defer ln.Close()

	port := ln.LocalAddr().(*net.UDPAddr).Port
	t.Setenv("FLEXCLIENT_DIRECT_PUNCH", "true")
	t.Setenv("FLEXCLIENT_DIRECT_PUNCH_PORT", strconv.Itoa(port))

	discovery := "discovery_protocol_version=3 serial=ABC123 ip=127.0.0.1 port=4992"
	maybeSendDirectRadioPunch("test-route", "ABC123", "127.0.0.1", nil, discovery)

	if err := ln.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline failed: %v", err)
	}
	buf := make([]byte, 128)
	n, addr, err := ln.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("ReadFromUDP failed: %v", err)
	}
	if got := string(buf[:n]); got != "FRNT_DIRECT_PUNCH" {
		t.Fatalf("unexpected punch payload %q", got)
	}
	if addr == nil || !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("unexpected source addr: %v", addr)
	}
}

func TestMaybeSendDirectRadioPunchDisabled(t *testing.T) {
	resetDirectPunchConns()
	t.Cleanup(resetDirectPunchConns)

	ln, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP failed: %v", err)
	}
	defer ln.Close()

	port := ln.LocalAddr().(*net.UDPAddr).Port
	t.Setenv("FLEXCLIENT_DIRECT_PUNCH", "false")
	t.Setenv("FLEXCLIENT_DIRECT_PUNCH_PORT", strconv.Itoa(port))

	discovery := "discovery_protocol_version=3 serial=ABC123 ip=127.0.0.1 port=4992"
	maybeSendDirectRadioPunch("test-route", "ABC123", "127.0.0.1", nil, discovery)

	if err := ln.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline failed: %v", err)
	}
	buf := make([]byte, 128)
	if n, _, err := ln.ReadFromUDP(buf); err == nil {
		t.Fatalf("unexpected punch payload %q", string(buf[:n]))
	}
}
