// FRNT discovery client for headless OpenWrt routers.
// Implements the HELLO/discovery wire exchange from the upstream flexclient.
// This intentionally does NOT proxy SmartSDR control or VITA data sessions.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/gopacket"
	"github.com/littleairmada/vrt"
)

const (
	version = "0.1.1"
	serverPort = 14992
	discoveryPort = 4992
	helloInterval = 30 * time.Second
	rebroadcastInterval = time.Second
	freshFor = 4 * time.Second
	expireAfter = 20 * time.Second
)

type cachedFrame struct {
	packet []byte
	lastSeen time.Time
}

type lanTarget struct {
	source net.IP
	broadcast net.IP
}

func main() {
	server := flag.String("server", "", "FRNT server IPv4 address reachable via NetBird")
	lanInterface := flag.String("lan-iface", "br-lan", "LAN interface used for local UDP 4992 broadcasts")
	vpnInterface := flag.String("vpn-iface", "wt0", "NetBird VPN interface; control traffic stays on this device")
	findNetBirdRoute := flag.Bool("netbird-routes", false, "discover a Flextool /32 server route using the NetBird CLI")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("frnt-openwrt-client", version)
		return
	}
	if flag.NArg() > 0 {
		log.Fatal("unexpected positional arguments")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *server != "" && *findNetBirdRoute {
		log.Fatal("configure either -server or -netbird-routes, not both")
	}
	var serverIP net.IP
	if *findNetBirdRoute {
		var err error
		serverIP, err = discoverServerFromNetBird(ctx)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("FRNT: selected NetBird Flextool route %s", serverIP)
	} else {
		serverIP = net.ParseIP(*server).To4()
		if serverIP == nil {
			log.Fatal("-server requires a valid FRNT server VPN IPv4, or use -netbird-routes")
		}
	}
	if err := run(ctx, serverIP, *lanInterface, *vpnInterface); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, serverIP net.IP, interfaceName, vpnInterface string) error {
	target, err := resolveLAN(interfaceName)
	if err != nil {
		return fmt.Errorf("unsafe LAN broadcast configuration: %w", err)
	}

	// SO_BROADCAST is required for directed-broadcast UDP on Linux.
	lc := net.ListenConfig{Control: func(_ string, _ string, raw syscall.RawConn) error {
		var socketErr error
		if err := raw.Control(func(fd uintptr) {
			socketErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
		}); err != nil {
			return err
		}
		return socketErr
	}}
	lanPacketConn, err := lc.ListenPacket(ctx, "udp4", net.JoinHostPort(target.source.String(), "0"))
	if err != nil {
		return fmt.Errorf("cannot bind LAN broadcast sender to %s: %w", target.source, err)
	}
	defer lanPacketConn.Close()
	lanDest := &net.UDPAddr{IP: target.broadcast, Port: discoveryPort}

	serverAddr := &net.UDPAddr{IP: serverIP, Port: serverPort}
	conn, err := dialViaVPN(ctx, serverIP, vpnInterface)
	if err != nil {
		return fmt.Errorf("FRNT NetBird connection: %w", err)
	}
	defer conn.Close()

	transportIP := conn.LocalAddr().(*net.UDPAddr).IP
	if transportIP == nil || transportIP.IsUnspecified() {
		return fmt.Errorf("no source IPv4 route to FRNT server %s", serverAddr)
	}
	hello := []byte(fmt.Sprintf("HELLO client_ip=%s client_version=openwrt-%s", transportIP, version))
	sendHello := func() {
		if _, err := conn.Write(hello); err != nil {
			log.Printf("FRNT HELLO send error: %v", err)
		}
	}
	sendBroadcast := func(payload []byte) {
		if _, err := lanPacketConn.WriteTo(payload, lanDest); err != nil {
			log.Printf("LAN discovery broadcast error: %v", err)
		}
	}

	log.Printf("FRNT client started: server=%s VPN=%s LAN=%s src=%s broadcast=%s:%d",
		serverAddr, vpnInterface, interfaceName, target.source, target.broadcast, discoveryPort)
	sendHello()
	nextHello := time.Now().Add(helloInterval)
	nextRebroadcast := time.Now().Add(rebroadcastInterval)
	cache := make(map[string]cachedFrame)
	buf := make([]byte, 8192)
	var frames, heartbeats uint64

	for {
		if err := ctx.Err(); err != nil {
			log.Printf("FRNT client stopped (discovery frames=%d heartbeats=%d)", frames, heartbeats)
			return nil
		}
		now := time.Now()
		if !now.Before(nextHello) {
			sendHello()
			nextHello = now.Add(helloInterval)
		}
		if !now.Before(nextRebroadcast) {
			for serial, frame := range cache {
				age := now.Sub(frame.lastSeen)
				if age > expireAfter {
					delete(cache, serial)
				} else if age <= freshFor {
					sendBroadcast(frame.packet)
				}
			}
			nextRebroadcast = now.Add(rebroadcastInterval)
		}

		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := conn.Read(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("FRNT read error: %v", err)
			continue
		}

		packet := buf[:n]
		if bytes.HasPrefix(packet, []byte("HEARTBEAT")) {
			heartbeats++
			continue
		}

		serial, radioIP, err := discoveryFields(packet)
		if err != nil {
			continue // Control packets and invalid VRT frames are not broadcast.
		}
		cache[strings.ToLower(serial)] = cachedFrame{packet: append([]byte(nil), packet...), lastSeen: time.Now()}
		sendBroadcast(packet)
		frames++
		if frames == 1 || frames%100 == 0 {
			log.Printf("FRNT discovery rx=%d serial=%s radio=%s -> %s:%d", frames, serial, radioIP, target.broadcast, discoveryPort)
		}
	}
}

// Validate the VRT frame and discovery metadata before forwarding any bytes
// onto the LAN. Match the upstream client's modern-discovery requirement.
func discoveryFields(packet []byte) (serial string, radioIP string, err error) {
	v := vrt.VRT{}
	if err := v.DecodeFromBytes(packet, gopacket.NilDecodeFeedback); err != nil {
		return "", "", err
	}
	message := strings.TrimSpace(string(bytes.TrimRight(v.Payload, "\x00")))
	if !strings.Contains(message, "discovery_protocol_version=") {
		return "", "", fmt.Errorf("not modern discovery")
	}
	fields := map[string]string{}
	for _, f := range strings.Fields(message) {
		kv := strings.SplitN(f, "=", 2)
		if len(kv) == 2 {
			fields[kv[0]] = kv[1]
		}
	}
	serial = strings.TrimSpace(fields["serial"])
	radioIP = strings.TrimSpace(fields["ip"])
	port, convErr := strconv.Atoi(fields["port"])
	if serial == "" || net.ParseIP(radioIP) == nil || convErr != nil || port <= 0 || port > 65535 {
		return "", "", fmt.Errorf("invalid discovery fields")
	}
	return serial, radioIP, nil
}

func resolveLAN(name string) (lanTarget, error) {
	if strings.TrimSpace(name) == "" {
		return lanTarget{}, fmt.Errorf("LAN interface must be named explicitly")
	}
	nic, err := net.InterfaceByName(name)
	if err != nil {
		return lanTarget{}, err
	}
	if nic.Flags&net.FlagUp == 0 || nic.Flags&net.FlagLoopback != 0 || nic.Flags&net.FlagPointToPoint != 0 {
		return lanTarget{}, fmt.Errorf("interface %s is not an active Ethernet/Wi-Fi LAN", name)
	}
	addrs, err := nic.Addrs()
	if err != nil {
		return lanTarget{}, err
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := n.IP.To4()
		if ip4 == nil || !ip4.IsPrivate() {
			continue
		}
		ones, bits := n.Mask.Size()
		if bits != 32 || ones >= 31 || ones < 1 {
			continue
		}
		mask := net.CIDRMask(ones, 32)
		bcast := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			bcast[i] = ip4[i] | ^mask[i]
		}
		return lanTarget{source: ip4, broadcast: bcast}, nil
	}
	return lanTarget{}, fmt.Errorf("interface %s has no suitable private IPv4 LAN address", name)
}
