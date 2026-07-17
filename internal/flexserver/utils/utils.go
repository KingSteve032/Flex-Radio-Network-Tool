package utils

import (
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/littleairmada/vrt"
)

const udpSocketBufferSize = 4 * 1024 * 1024

type NetbirdApi struct {
	Password string
	Url      string
}

var (
	vitaSendLogMu            sync.Mutex
	vitaSendLastLogByID      = map[string]time.Time{}
	vitaSendLastPacketByID   = map[string]time.Time{}
	vitaSendLastLogCountByID = map[string]uint64{}
	vitaSendCountByID        = map[string]uint64{}
	vitaSendBytesByID        = map[string]uint64{}
	vitaSendLastLogBytesByID = map[string]uint64{}
	vitaSendMaxGapByID       = map[string]time.Duration{}
)

type NetInteface struct {
	Name       string
	IPAddress  net.IP
	MACAddress net.HardwareAddr
}

type ConfigOptions struct {
	Mode                  string
	PcapFile              string
	ListenInterface       string      // interface to capture from
	SendNetworkInterface  NetInteface // interface to send from
	NetworkInteface       NetInteface // legacy field (keep for compatibility)
	Clients               []net.IP
	EnableBroadcast       bool
	EnableDebug           bool
	EnableDeleteUsers     bool
	BPFFilter             string
	NetbirdApiConnection  NetbirdApi
	BroadcastPort         int
	DiscoveryDelaySeconds int
	SyncIntervalSeconds   int
	ClientAuthMode        string
	EnableVitaProxy       bool
	VitaProxyPort         int
	ProxyBasePort         int
	ProxyLANSourceIPs     []net.IP
	MultiProxy            bool
	IgnoreRadios          []string

	// shared UDP socket used to reply to client-initiated connections
	SendConn *net.UDPConn
}

type VpnRouteRow struct {
	AccessiblePeersCount int    `json:"accessible_peers_count"`
	ApprovalRequired     bool   `json:"approval_required"`
	CityName             string `json:"city_name"`
	Connected            bool   `json:"connected"`
	ConnectionIP         string `json:"connection_ip"`
	CountryCode          string `json:"country_code"`
	DNSLabel             string `json:"dns_label"`
	GeoNameID            int    `json:"geoname_id"`
	Groups               []struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		PeersCount int    `json:"peers_count"`
	} `json:"groups"`
	Hostname                    string `json:"hostname"`
	ID                          string `json:"id"`
	InactivityExpirationEnabled bool   `json:"inactivity_expiration_enabled"`
	IP                          string `json:"ip"`
	KernelVersion               string `json:"kernel_version"`
	LastLogin                   string `json:"last_login"`
	LastSeen                    string `json:"last_seen"`
	LoginExpirationEnabled      bool   `json:"login_expiration_enabled"`
	LoginExpired                bool   `json:"login_expired"`
	Name                        string `json:"name"`
	OS                          string `json:"os"`
	SerialNumber                string `json:"serial_number"`
	SSHEnabled                  bool   `json:"ssh_enabled"`
	UIVersion                   string `json:"ui_version"`
	UserID                      string `json:"user_id"`
	Version                     string `json:"version"`
}

// ---------------------------------------------------------------------
// Client registration tracking
// ---------------------------------------------------------------------

type ClientInfo struct {
	Addr         *net.UDPAddr
	IP           string
	Version      string
	RegisteredAt time.Time
	LastSeen     time.Time
}

var activeClients sync.Map // key: client_ip -> *ClientInfo

const (
	ClientAuthModeDB         = "db"
	ClientAuthModeRegistered = "registered"
	heartbeatInterval        = 30 * time.Second
	clientRegistrationTTL    = 2 * time.Minute
)

// GetNetworkInterfaceByName returns details about a single user provided interface
func GetNetworkInterfaceByName(name string) {
	netInterface, err := net.InterfaceByName(name)
	if err != nil {
		fmt.Println("Error accessing network interface: ", err)
		return
	}
	header := "Interface Id\tInterface Name\tHardware Address\tIP Addresses\n============\t==============\t================\t============"
	var addrs_output string
	if addrs, err := netInterface.Addrs(); err == nil {
		for _, addr := range addrs {
			addrs_output = addrs_output + " " + addr.String()
		}
	}

	output := header + "\n" +
		strconv.FormatInt(int64(netInterface.Index), 10) + "\t" +
		netInterface.Name + "\t" +
		netInterface.HardwareAddr.String() + "\t" +
		addrs_output + "\n"

	fmt.Print(output)
}

// ValidateNetworkInterfaceByName returns details about a single user provided interface
func ValidateNetworkInterfaceByName(name string) (NetInteface, error) {
	netInterface, err := net.InterfaceByName(name)
	ni := NetInteface{}

	if err != nil {
		return ni, err
	}

	ni.Name = netInterface.Name
	ni.MACAddress = netInterface.HardwareAddr
	if addrs, err := netInterface.Addrs(); err == nil {
		for _, addr := range addrs {
			if ipv4Addr := addr.(*net.IPNet).IP.To4(); ipv4Addr != nil {
				ni.IPAddress = ipv4Addr
				return ni, nil
			}
		}
	}

	return ni, nil
}

// PrintVrtPacket dumps VRT packet contents for debugging
func PrintVrtPacket(vrt_packet vrt.VRT) {
	fmt.Println("VRT Packet Header Type: ", vrt_packet.Header.Type)
	fmt.Println("VRT Packet Header ClassID Present?: ", vrt_packet.Header.C)
	fmt.Println("VRT Packet Header Trailer Present?: ", vrt_packet.Header.T)
	fmt.Println("VRT Packet Header TSI: ", vrt_packet.Header.TSI)
	fmt.Println("VRT Packet Header TSF: ", vrt_packet.Header.TSF)
	fmt.Println("VRT Packet Header PacketCount: ", vrt_packet.Header.PacketCount)
	fmt.Println("VRT Packet Header PacketSize: ", vrt_packet.Header.PacketSize)
	fmt.Println("VRT Packet StreamId: ", vrt_packet.StreamID)
	fmt.Println("VRT Packet ClassID OUI: ", vrt_packet.ClassID.OUI)
	fmt.Println("VRT Packet ClassID PacketClassCode: ", vrt_packet.ClassID.PacketClassCode)
	fmt.Println("VRT Packet ClassID InformationClassCode: ", vrt_packet.ClassID.InformationClassCode)
	fmt.Println("VRT Packet TimestampInt: ", vrt_packet.TimestampInt)
	fmt.Println("VRT Packet TimestampFrac: ", vrt_packet.TimestampFrac)
	fmt.Println("VRT Packet Payload length: ", len(vrt_packet.Payload))
	fmt.Println("VRT Packet Contents hexdump:")
	fmt.Println(hex.Dump(vrt_packet.Contents))
	fmt.Println("VRT Packet Payload hexdump:")
	fmt.Println(hex.Dump(vrt_packet.Payload))
}

func NormalizeClientAuthMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", ClientAuthModeDB, "database", "netbird":
		return ClientAuthModeDB
	case ClientAuthModeRegistered, "registration", "manual", "open", "none":
		return ClientAuthModeRegistered
	default:
		return ClientAuthModeDB
	}
}

// ---------------------------------------------------------------------
// Authorization helper
// ---------------------------------------------------------------------

func isAuthorizedClientInDB(ip string) bool {
	u, err := UsersDb()
	if err != nil {
		log.Printf("[AUTH] error opening db for ip %s: %v\n", ip, err)
		return false
	}
	ok, err := u.HasIP(ip)
	if err != nil {
		log.Printf("[AUTH] error checking ip %s: %v\n", ip, err)
		return false
	}
	return ok
}

func isAuthorizedClient(ip string, co *ConfigOptions) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false
	}
	if co != nil && NormalizeClientAuthMode(co.ClientAuthMode) == ClientAuthModeRegistered {
		return true
	}
	return isAuthorizedClientInDB(ip)
}

func isIgnoredClient(ip string, co ConfigOptions) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return true
	}
	for _, ignoreIP := range co.IgnoreRadios {
		if ip == strings.TrimSpace(ignoreIP) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// HELLO tracking: client_ip + client_version
// ---------------------------------------------------------------------
//
// Expected HELLO format from flexclient:
//
//	"HELLO client_ip=100.85.50.11 client_version=0.1.0"
func registerClient(addr *net.UDPAddr, payload []byte, co *ConfigOptions) {
	if co == nil {
		return
	}
	debug := co.EnableDebug
	if HandleClientVitaTX(addr.IP.String(), *co, payload) {
		return
	}

	line := strings.TrimSpace(string(payload))

	if strings.HasPrefix(line, "PROXY_SELECT") {
		clientIP := addr.IP.String()
		serial := ""
		parts := strings.Fields(line)
		for _, p := range parts[1:] {
			kv := strings.SplitN(p, "=", 2)
			if len(kv) != 2 {
				continue
			}
			key := kv[0]
			val := kv[1]
			switch key {
			case "client_ip":
				if val != "" {
					clientIP = val
				}
			case "serial":
				serial = val
			}
		}

		if clientIP != "" && isAuthorizedClient(clientIP, co) && serial != "" {
			SetClientSelectedProxySerial(clientIP, serial)
			if debug {
				fmt.Printf("[PROXY] select client_ip=%s serial=%s\n", clientIP, serial)
			}
		} else if debug {
			fmt.Printf("[PROXY] ignored PROXY_SELECT from %s raw=%q\n", addr.String(), line)
		}
		return
	}

	if !strings.HasPrefix(line, "HELLO") {
		if debug {
			fmt.Printf("[CONTROL] Unknown control packet from %s: %q\n", addr.String(), line)
		}
		return
	}

	ci := &ClientInfo{
		Addr:         addr,
		IP:           addr.IP.String(), // default to socket source IP
		Version:      "",
		RegisteredAt: time.Now(),
		LastSeen:     time.Now(),
	}

	parts := strings.Fields(line)
	// parts[0] = "HELLO"
	for _, p := range parts[1:] {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := kv[0]
		val := kv[1]

		switch key {
		case "client_ip":
			if val != "" {
				ci.IP = val
			}
		case "client_version":
			ci.Version = val
		}
	}

	// Authorize against VPN DB
	if !isAuthorizedClient(ci.IP, co) {
		if debug {
			fmt.Printf("[DENY] HELLO from %s (client_ip=%s, version=%s) auth_mode=%s\n",
				addr.String(), ci.IP, ci.Version, NormalizeClientAuthMode(co.ClientAuthMode))
		}
		return
	}

	if existing, ok := getClientInfo(ci.IP); ok && existing.RegisteredAt.IsZero() == false {
		ci.RegisteredAt = existing.RegisteredAt
	}
	activeClients.Store(ci.IP, ci)

	if debug {
		fmt.Printf("[REGISTER] HELLO from %s (client_ip=%s, version=%s, raw=%q)\n",
			addr.String(), ci.IP, ci.Version, line)
	}
}

func getClientInfo(ip string) (*ClientInfo, bool) {
	if v, ok := activeClients.Load(ip); ok {
		if ci, ok2 := v.(*ClientInfo); ok2 {
			if !ci.LastSeen.IsZero() && time.Since(ci.LastSeen) > clientRegistrationTTL {
				activeClients.Delete(ip)
				return nil, false
			}
			return ci, true
		}
	}
	return nil, false
}

func getClientAddr(ip string) (*net.UDPAddr, bool) {
	if ci, ok := getClientInfo(ip); ok && ci.Addr != nil {
		return ci.Addr, true
	}
	return nil, false
}

type discoveryTarget struct {
	IP          string
	ConnectedAt time.Time
}

func registeredDiscoveryTargets(co ConfigOptions) []discoveryTarget {
	now := time.Now()
	var out []discoveryTarget
	activeClients.Range(func(key, value any) bool {
		ci, ok := value.(*ClientInfo)
		if !ok || ci == nil || ci.Addr == nil || strings.TrimSpace(ci.IP) == "" {
			return true
		}
		if !ci.LastSeen.IsZero() && now.Sub(ci.LastSeen) > clientRegistrationTTL {
			activeClients.Delete(key)
			return true
		}
		if isIgnoredClient(ci.IP, co) {
			if co.EnableDebug {
				fmt.Println("[IGNORE] Skipping registered client", ci.IP)
			}
			return true
		}
		connectedAt := ci.RegisteredAt
		if connectedAt.IsZero() {
			connectedAt = ci.LastSeen
		}
		out = append(out, discoveryTarget{IP: ci.IP, ConnectedAt: connectedAt})
		return true
	})
	return out
}

func dbDiscoveryTargets(co ConfigOptions) ([]discoveryTarget, error) {
	u, err := UsersDb()
	if err != nil {
		return nil, fmt.Errorf("error accessing db: %w", err)
	}

	clientIPs, err := u.GetUserIpAddresses()
	if err != nil {
		return nil, fmt.Errorf("error retrieving vpn client ips from sqlite db: %w", err)
	}

	out := make([]discoveryTarget, 0, len(clientIPs))
	for _, clientIP := range clientIPs {
		if isIgnoredClient(clientIP, co) {
			if co.EnableDebug {
				fmt.Println("[IGNORE] Skipping discovery for", clientIP)
			}
			continue
		}
		connectedAt, _ := u.GetConnectedTime(clientIP)
		out = append(out, discoveryTarget{IP: clientIP, ConnectedAt: connectedAt})
	}
	return out, nil
}

// StartClientRegistrationServer listens on SendNetworkInterface:BroadcastPort
// and records any flexclient that sends us a HELLO.
// Also starts a heartbeat loop that sends HEARTBEAT packets back to clients.
func StartClientRegistrationServer(co *ConfigOptions) error {
	localAddr := &net.UDPAddr{
		IP:   co.SendNetworkInterface.IPAddress,
		Port: co.BroadcastPort,
	}

	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		return fmt.Errorf("failed to start client registration listener on %s:%d: %w",
			co.SendNetworkInterface.IPAddress.String(), co.BroadcastPort, err)
	}
	_ = conn.SetReadBuffer(udpSocketBufferSize)
	_ = conn.SetWriteBuffer(udpSocketBufferSize)

	co.SendConn = conn

	// Reader goroutine (HELLOs)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			registerClient(addr, buf[:n], co)
		}
	}()

	// Heartbeat goroutine
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		for range ticker.C {
			if co.SendConn == nil {
				continue
			}
			activeClients.Range(func(key, value any) bool {
				ci, ok := value.(*ClientInfo)
				if !ok || ci.Addr == nil {
					return true
				}
				msg := fmt.Sprintf("HEARTBEAT client_ip=%s ts=%s",
					ci.IP, time.Now().UTC().Format(time.RFC3339))
				if co.EnableDebug {
					fmt.Printf("[HEARTBEAT] to %s (%s)\n", ci.IP, ci.Addr.String())
				}
				if _, err := co.SendConn.WriteToUDP([]byte(msg), ci.Addr); err != nil && co.EnableDebug {
					fmt.Printf("[HEARTBEAT] error sending to %s: %v\n", ci.IP, err)
				}
				return true
			})
		}
	}()

	if co.EnableDebug {
		fmt.Printf("Client registration server listening on %s:%d\n",
			co.SendNetworkInterface.IPAddress.String(), co.BroadcastPort)
	}

	return nil
}

// ---------------------------------------------------------------------
// Discovery packet send pipeline
// ---------------------------------------------------------------------

// MaybeSendDiscoveryPacket regenerates and sends the Discovery Packet if EnableBroadcast is true
func MaybeSendDiscoveryPacket(co ConfigOptions, p vrt.VRT) {
	if !co.EnableBroadcast {
		fmt.Println("Send Discovery Packet Disabled")
		return
	}

	// Serialize packet to bytes
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: false, ComputeChecksums: false}
	if err := p.SerializeTo(buf, opts); err != nil {
		fmt.Println("Unable to serialize VRT packet:", err)
		return
	}

	var targets []discoveryTarget
	if NormalizeClientAuthMode(co.ClientAuthMode) == ClientAuthModeRegistered {
		targets = registeredDiscoveryTargets(co)
	} else {
		var err error
		targets, err = dbDiscoveryTargets(co)
		if err != nil {
			fmt.Println(err)
			return
		}
	}

	for _, target := range targets {
		go func(target discoveryTarget) {
			delay := time.Duration(co.DiscoveryDelaySeconds) * time.Second
			if !target.ConnectedAt.IsZero() {
				elapsed := time.Since(target.ConnectedAt)
				if elapsed < delay {
					wait := delay - elapsed
					fmt.Printf("[DELAY] Waiting %v before sending discovery to %s\n", wait, target.IP)
					time.AfterFunc(wait, func() {
						sendDiscoveryPacketTo(target.IP, co, buf.Bytes())
					})
					return
				} else if co.EnableDebug {
					fmt.Printf("[SEND NOW] %s connected %v ago, delay expired\n", target.IP, elapsed)
				}
			}
			sendDiscoveryPacketTo(target.IP, co, buf.Bytes())
		}(target)
	}
}

func sendDiscoveryPacketTo(clientIp string, co ConfigOptions, payload []byte) {
	if !SendPayloadToAuthorizedRegisteredClient(clientIp, co, payload, "DISCOVERY") && co.EnableDebug {
		fmt.Printf("[SKIP] No active flexclient registration for %s; not sending discovery\n", clientIp)
	}
}

func recordVitaSendMetric(clientIP, packetType string, payloadLen int) {
	if !strings.HasPrefix(packetType, "VITA") {
		return
	}
	key := packetType + "|" + strings.TrimSpace(clientIP)
	now := time.Now()

	vitaSendLogMu.Lock()
	defer vitaSendLogMu.Unlock()

	vitaSendCountByID[key]++
	vitaSendBytesByID[key] += uint64(payloadLen)
	if lastPacket := vitaSendLastPacketByID[key]; !lastPacket.IsZero() {
		if gap := now.Sub(lastPacket); gap > vitaSendMaxGapByID[key] {
			vitaSendMaxGapByID[key] = gap
		}
	}
	vitaSendLastPacketByID[key] = now

	last := vitaSendLastLogByID[key]
	if now.Sub(last) < 5*time.Second {
		return
	}
	elapsed := now.Sub(last).Seconds()
	if last.IsZero() || elapsed <= 0 {
		elapsed = 5
	}
	totalPackets := vitaSendCountByID[key]
	totalBytes := vitaSendBytesByID[key]
	intervalPackets := totalPackets - vitaSendLastLogCountByID[key]
	intervalBytes := totalBytes - vitaSendLastLogBytesByID[key]
	pps := float64(intervalPackets) / elapsed
	maxGap := vitaSendMaxGapByID[key]

	vitaSendLastLogByID[key] = now
	vitaSendLastLogCountByID[key] = totalPackets
	vitaSendLastLogBytesByID[key] = totalBytes
	vitaSendMaxGapByID[key] = 0

	fmt.Printf("[VITA-METRIC] send type=%s client=%s payload=%d total=%d pps=%.1f bytes=%d max_gap=%s\n",
		packetType, clientIP, payloadLen, totalPackets, pps, intervalBytes, maxGap.Truncate(time.Millisecond))
}

// SendPayloadToAuthorizedRegisteredClient sends a payload to a registered flexclient
// identified by its NetBird IP. Returns true when a datagram was sent.
func SendPayloadToAuthorizedRegisteredClient(clientIP string, co ConfigOptions, payload []byte, packetType string) bool {
	addr, ok := getClientAddr(clientIP)
	if !ok || co.SendConn == nil {
		return false
	}

	if co.EnableDebug {
		if ci, ok := getClientInfo(clientIP); ok {
			fmt.Printf("Sending %s packet to %s (version=%s) via %s -> %s\n",
				packetType,
				clientIP,
				ci.Version,
				co.SendNetworkInterface.IPAddress.String(),
				addr.String(),
			)
		} else {
			fmt.Printf("Sending %s packet to %s via %s -> %s\n",
				packetType,
				clientIP,
				co.SendNetworkInterface.IPAddress.String(),
				addr.String(),
			)
		}
	}

	if _, err := co.SendConn.WriteToUDP(payload, addr); err != nil {
		fmt.Println("error sending udp packet via existing connection:", err)
		return false
	}
	recordVitaSendMetric(clientIP, packetType, len(payload))
	return true
}
