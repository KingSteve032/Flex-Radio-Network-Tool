package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// discoverServerFromNetBird mirrors the upstream desktop client's legacy
// NetBird "routes list" discovery. The route name must contain "flextool"
// and its destination must be exactly one IPv4 host (/32).
// Some older OpenWrt NetBird versions lack this CLI command. For them use
// the explicit FRNT server VPN IP in UCI instead.
func discoverServerFromNetBird(ctx context.Context) (net.IP, error) {
	discoveryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(discoveryCtx, "netbird", "routes", "list")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("netbird routes list unavailable: %w (%s); configure a server VPN IP manually", err, strings.TrimSpace(string(out)))
	}
	return parseNetBirdRoutes(out)
}

func parseNetBirdRoutes(out []byte) (net.IP, error) {
	type route struct {
		id, network, status string
	}
	var entries []route
	var current *route
	commit := func() {
		if current != nil {
			entries = append(entries, *current)
			current = nil
		}
	}
	s := bufio.NewScanner(bytes.NewReader(out))
	for s.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.Text()), "- "))
		switch {
		case strings.HasPrefix(line, "ID:"):
			commit()
			current = &route{id: strings.TrimSpace(strings.TrimPrefix(line, "ID:"))}
		case current != nil && strings.HasPrefix(line, "Network:"):
			current.network = strings.TrimSpace(strings.TrimPrefix(line, "Network:"))
		case current != nil && strings.HasPrefix(line, "Status:"):
			current.status = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "Status:")))
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	commit()
	ips := make(map[string]net.IP)
	for _, r := range entries {
		if !strings.Contains(strings.ToLower(r.id), "flextool") {
			continue
		}
		if r.status != "" && r.status != "selected" {
			continue
		}
		ip, network, err := net.ParseCIDR(r.network)
		if err != nil {
			continue
		}
		ones, bits := network.Mask.Size()
		if bits != 32 || ones != 32 || ip.To4() == nil {
			continue // FRNT server discovery needs one host, not a subnet.
		}
		ips[ip.String()] = ip.To4()
	}
	switch len(ips) {
	case 0:
		return nil, fmt.Errorf("no selected Flextool /32 route found in NetBird; configure a server VPN IP manually")
	case 1:
		for _, ip := range ips {
			return ip, nil
		}
	}
	return nil, fmt.Errorf("multiple Flextool routes found: choose the FRNT server VPN IP explicitly")
}

// dialViaVPN follows NetBird's Linux policy routing for network-resource /32
// routes (10.x), matching the upstream desktop client. SO_BINDTODEVICE can
// bypass policy routing and cause "network is unreachable" for these routes.
// Fail closed before sending HELLO if the selected source is not assigned to
// the NetBird interface; that prevents the control channel leaking to WAN.
func dialViaVPN(ctx context.Context, serverIP net.IP, vpnInterface string) (*net.UDPConn, error) {
	if vpnInterface == "" {
		return nil, fmt.Errorf("VPN interface must be configured (normally wt0)")
	}
	iface, err := net.InterfaceByName(vpnInterface)
	if err != nil {
		return nil, fmt.Errorf("NetBird interface %q unavailable: %w", vpnInterface, err)
	}
	if iface.Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("NetBird interface %q is down", vpnInterface)
	}
	addr := net.JoinHostPort(serverIP.String(), fmt.Sprint(serverPort))
	// Unmarked sockets must follow the normal NetBird routing policy.
	c, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("cannot route to FRNT %s: %w", addr, err)
	}
	conn, ok := c.(*net.UDPConn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("unexpected NetBird connection type")
	}
	local := conn.LocalAddr().(*net.UDPAddr).IP.To4()
	addrs, err := iface.Addrs()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !sourceBelongsToVPN(local, addrs) {
		_ = conn.Close()
		return nil, fmt.Errorf("route to FRNT %s selected source %s outside NetBird interface %s; no HELLO sent (check ip route get %s)", addr, local, vpnInterface, serverIP)
	}
	return conn, nil
}

func sourceBelongsToVPN(local net.IP, addrs []net.Addr) bool {
	if local == nil || local.To4() == nil {
		return false
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if ok && ipNet.IP.To4() != nil && ipNet.IP.To4().Equal(local.To4()) {
			return true
		}
	}
	return false
}
