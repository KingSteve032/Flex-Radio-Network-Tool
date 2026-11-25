package main

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

func TestSourceBelongsToVPN(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("100.85.12.187"), Mask: net.CIDRMask(16, 32)},
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
	}
	for _, tc := range []struct {
		name string
		ip   net.IP
		want bool
	}{
		{"vpn source allowed", net.ParseIP("100.85.12.187"), true},
		{"same NetBird subnet but unassigned denied", net.ParseIP("100.85.50.97"), false},
		{"WAN source denied", net.ParseIP("192.168.1.123"), false},
		{"LAN source denied", net.ParseIP("192.168.9.1"), false},
		{"nil source denied", nil, false},
		{"IPv6 source denied", net.ParseIP("fe80::1"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceBelongsToVPN(tc.ip, addrs); got != tc.want {
				t.Fatalf("sourceBelongsToVPN(%v) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestNetworkUnavailableReopensSocket(t *testing.T) {
	for _, code := range []error{
		syscall.ENETUNREACH,
		syscall.EHOSTUNREACH,
		syscall.ENETDOWN,
		syscall.ENODEV,
		syscall.EADDRNOTAVAIL,
	} {
		if !isNetworkUnavailable(fmt.Errorf("udp: %w", code)) {
			t.Fatalf("expected restart on error %v", code)
		}
	}
	for _, code := range []error{nil, errors.New("timeout"), syscall.ECONNREFUSED} {
		if isNetworkUnavailable(code) {
			t.Fatalf("unexpected restart on %v", code)
		}
	}
}
