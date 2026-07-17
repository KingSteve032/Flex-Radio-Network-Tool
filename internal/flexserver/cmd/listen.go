/*
Copyright © 2023 Blair Gillam <ns1h@airmada.net>
Reconfigured for Netbird by Steven Griggs <kc4caw@w4car.org>
Enhanced for separate listen/send interfaces and flexclient handshake via utils
*/
package cmd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexserver/utils"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/littleairmada/vrt"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	defaultVitaProxyPort = 4991
	vitaProxyPacketMagic = "VITAP1"
)

// ViperValidateListenConfigOptions validates configuration for listen mode
func ViperValidateListenConfigOptions(mode string, c *viper.Viper) (co utils.ConfigOptions, err error) {
	flag_listeniface := c.GetString("LISTEN_INTERFACE") // interface to capture from (LAN)
	flag_sendiface := c.GetString("SEND_INTERFACE")     // interface to send from (NetBird/VPN)
	flag_broadcast := c.GetBool("broadcast")
	flag_broadcastport := c.GetInt("port")
	flag_debug := c.GetBool("debug")
	flag_bpffilter := c.GetString("filter")
	delay := c.GetInt("DISCOVERY_DELAY_SECONDS")
	syncIntervalSeconds := c.GetInt("SYNC_INTERVAL_SECONDS")
	clientAuthMode := c.GetString("CLIENT_AUTH_MODE")
	enableVitaProxy := c.GetBool("ENABLE_VITA_PROXY")
	vitaProxyPort := c.GetInt("VITA_PROXY_PORT")
	proxyBasePort := c.GetInt("PROXY_BASE_PORT")
	proxyLANSourceIPsRaw := c.GetString("PROXY_LAN_SOURCE_IPS")
	multiProxy := c.GetBool("MULTI_PROXY")

	co = utils.ConfigOptions{}

	// validate MODE
	switch mode {
	case "listen":
		co.Mode = "listen"
	default:
		return utils.ConfigOptions{}, fmt.Errorf("invalid mode %s", mode)
	}

	// validate broadcast port
	if math.Signbit(float64(flag_broadcastport)) || flag_broadcastport >= 65536 {
		return utils.ConfigOptions{}, fmt.Errorf("port number must be between 0 and 65535")
	}
	co.BroadcastPort = flag_broadcastport

	co.EnableBroadcast = flag_broadcast
	co.EnableDebug = flag_debug
	co.DiscoveryDelaySeconds = delay
	co.ClientAuthMode = utils.NormalizeClientAuthMode(clientAuthMode)
	if syncIntervalSeconds < 0 {
		return utils.ConfigOptions{}, fmt.Errorf("SYNC_INTERVAL_SECONDS must be 0 or greater")
	}
	co.SyncIntervalSeconds = syncIntervalSeconds
	co.EnableVitaProxy = enableVitaProxy
	co.MultiProxy = multiProxy
	if proxyBasePort <= 0 {
		proxyBasePort = 30000
	}
	co.ProxyBasePort = proxyBasePort
	proxyLANSourceIPs, err := parseProxyLANSourceIPs(proxyLANSourceIPsRaw)
	if err != nil {
		return utils.ConfigOptions{}, err
	}
	co.ProxyLANSourceIPs = proxyLANSourceIPs

	if vitaProxyPort == 0 {
		vitaProxyPort = defaultVitaProxyPort
	}
	if vitaProxyPort <= 0 || vitaProxyPort >= 65536 {
		return utils.ConfigOptions{}, fmt.Errorf("VITA_PROXY_PORT must be between 1 and 65535")
	}
	co.VitaProxyPort = vitaProxyPort

	// validate listen interface
	if flag_listeniface == "" {
		return co, fmt.Errorf("LISTEN_INTERFACE not specified")
	}
	tempListenIface, err := utils.ValidateNetworkInterfaceByName(flag_listeniface)
	if err != nil {
		return co, fmt.Errorf("error validating listen interface: %v", err)
	}
	co.ListenInterface = tempListenIface.Name

	// validate send interface (NetBird/VPN side)
	if flag_sendiface == "" {
		return co, fmt.Errorf("SEND_INTERFACE not specified")
	}
	tempSendIface, err := utils.ValidateNetworkInterfaceByName(flag_sendiface)
	if err != nil {
		return co, fmt.Errorf("error validating send interface: %v", err)
	}
	co.SendNetworkInterface = tempSendIface

	// BPF filter
	if flag_bpffilter != "" {
		co.BPFFilter = flag_bpffilter
	} else {
		co.BPFFilter = "udp and port 4992 and dst host 255.255.255.255"
	}

	// parse ignore radios (used as ignore list for client IPs)
	ignore := c.GetString("IGNORE_RADIOS")
	if ignore != "" {
		co.IgnoreRadios = strings.Split(ignore, ",")
	}

	if co.EnableDebug {
		fmt.Println("Listening on interface:", co.ListenInterface)
		fmt.Println("Sending on interface:", co.SendNetworkInterface.Name, co.SendNetworkInterface.IPAddress)
		fmt.Println("Discovery delay:", co.DiscoveryDelaySeconds, "seconds")
		fmt.Println("Client auth mode:", co.ClientAuthMode)
		fmt.Println("Sync interval:", co.SyncIntervalSeconds, "seconds")
		fmt.Println("VITA proxy enabled:", co.EnableVitaProxy)
		fmt.Println("VITA proxy port:", co.VitaProxyPort)
		fmt.Println("Ignore radios/clients:", co.IgnoreRadios)
		fmt.Println("Broadcast/Server port:", co.BroadcastPort)
	}

	return co, nil
}

func parseProxyLANSourceIPs(raw string) ([]net.IP, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	seen := map[string]bool{}
	var out []net.IP
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		ip := net.ParseIP(tok)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("PROXY_LAN_SOURCE_IPS contains invalid IPv4 address %q", tok)
		}
		ip = ip.To4()
		key := ip.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ip)
	}
	return out, nil
}

func formatIPList(ips []net.IP) string {
	if len(ips) == 0 {
		return "(default route)"
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return strings.Join(out, ",")
}

func discoveryField(payload, key string) string {
	prefix := key + "="
	for _, tok := range strings.Fields(payload) {
		if strings.HasPrefix(tok, prefix) {
			return strings.TrimPrefix(tok, prefix)
		}
	}
	return ""
}

func extractDiscoveredRadio(vrtStruct vrt.VRT) (serial, ip string, port int, ok bool) {
	payload := strings.TrimSpace(string(bytes.TrimRight(vrtStruct.Payload, "\x00")))
	if payload == "" {
		return "", "", 0, false
	}

	serial = discoveryField(payload, "serial")
	ip = discoveryField(payload, "ip")
	portRaw := discoveryField(payload, "port")
	if serial == "" || ip == "" || portRaw == "" {
		return "", "", 0, false
	}

	p, err := strconv.Atoi(portRaw)
	if err != nil || p <= 0 || p >= 65536 {
		return "", "", 0, false
	}
	return serial, ip, p, true
}

// ListenForPackets listens for FlexRadio discovery packets and forwards them
// to any registered (HELLO) flexclients using utils.MaybeSendDiscoveryPacket.
func ListenForPackets(co utils.ConfigOptions) error {
	handle, err := pcap.OpenLive(co.ListenInterface, 1600, false, pcap.BlockForever)
	if err != nil {
		return fmt.Errorf("error opening listen interface: %v", err)
	}
	defer handle.Close()

	if err := handle.SetBPFFilter(co.BPFFilter); err != nil {
		return fmt.Errorf("error setting BPF filter: %v", err)
	}

	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	for packet := range packetSource.Packets() {
		udpLayer := packet.Layer(layers.LayerTypeUDP)
		if udpLayer == nil {
			continue
		}

		vrtStruct := vrt.VRT{}
		udp, _ := udpLayer.(*layers.UDP)
		if udp == nil || udp.Payload == nil || len(udpLayer.LayerPayload()) == 0 {
			continue
		}

		if err := vrtStruct.DecodeFromBytes(udp.Payload, gopacket.NilDecodeFeedback); err != nil {
			fmt.Println("Error decoding UDP packet as VRT:", err)
			continue
		}

		if co.EnableDebug {
			utils.PrintVrtPacket(vrtStruct)
		}

		if serial, ip, port, ok := extractDiscoveredRadio(vrtStruct); ok {
			utils.RegisterDiscoveredRadio(serial, ip, port, co)
		}

		// Use utils pipeline: DB-gated, HELLO-registered, heartbeat-backed
		utils.MaybeSendDiscoveryPacket(co, vrtStruct)
	}

	return nil
}

func buildVitaProxyPayload(dstPort uint16, payload []byte) []byte {
	out := make([]byte, 0, len(vitaProxyPacketMagic)+2+len(payload))
	out = append(out, []byte(vitaProxyPacketMagic)...)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, dstPort)
	out = append(out, portBytes...)
	out = append(out, payload...)
	return out
}

// ListenForVitaStreamPackets captures radio VITA UDP frames from the LAN side and
// relays them to registered flexclients through the control channel.
func ListenForVitaStreamPackets(co utils.ConfigOptions) error {
	// Radios are not fully consistent about UDP source/destination stream ports,
	// especially across firmware generations. Keep capture broad and rely on
	// session/client routing logic below to forward only relevant packets.
	// Exclude 4992 discovery traffic since it is handled by the discovery path.
	filter := "udp and not port 4992"
	handle, err := pcap.OpenLive(co.ListenInterface, 1600, false, pcap.BlockForever)
	if err != nil {
		return fmt.Errorf("error opening listen interface for VITA proxy: %v", err)
	}
	defer handle.Close()

	if err := handle.SetBPFFilter(filter); err != nil {
		return fmt.Errorf("error setting VITA proxy BPF filter: %v", err)
	}

	if co.EnableDebug {
		fmt.Printf("[VITA] proxy capture active on %s with filter %q\n", co.ListenInterface, filter)
	}

	packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
	for packet := range packetSource.Packets() {
		ipLayer := packet.Layer(layers.LayerTypeIPv4)
		udpLayer := packet.Layer(layers.LayerTypeUDP)
		if ipLayer == nil || udpLayer == nil {
			continue
		}

		ipv4, ok := ipLayer.(*layers.IPv4)
		if !ok || ipv4 == nil {
			continue
		}
		udp, ok := udpLayer.(*layers.UDP)
		if !ok || udp == nil || len(udp.Payload) == 0 {
			continue
		}
		radioIP := ipv4.SrcIP.String()
		if !utils.IsKnownRadioIP(radioIP) {
			continue
		}

		// Direct mode: packet was destined to client NetBird IP.
		clientIP := ipv4.DstIP.String()
		proxyPayload := buildVitaProxyPayload(uint16(udp.DstPort), udp.Payload)
		if utils.SendPayloadToAuthorizedRegisteredClient(clientIP, co, proxyPayload, "VITA") {
			continue
		}

		// Proxy mode: packet is destined to a server LAN IP, so route by the
		// proxied TCP session whose source LAN IP and UDP port match it.
		proxyTargets := utils.GetVitaProxyTargetsForDestination(radioIP, ipv4.DstIP.String(), int(udp.DstPort))
		if len(proxyTargets) > 0 {
			sent := false
			for _, target := range proxyTargets {
				proxyModePayload := buildVitaProxyPayload(uint16(target.Port), udp.Payload)
				if utils.SendPayloadToAuthorizedRegisteredClient(target.ClientIP, co, proxyModePayload, "VITA-PROXY") {
					sent = true
				}
			}
			if sent {
				continue
			}
		}

		if co.EnableDebug {
			fmt.Printf("[VITA] skipping packet src=%s dst=%s (no active registered client/session)\n", radioIP, clientIP)
		}
	}

	return nil
}

func runPeriodicSync(syncConfig utils.ConfigOptions, interval time.Duration) {
	syncOnce := func() {
		count, err := SyncUsersFromNetBird(syncConfig, false)
		if err != nil {
			fmt.Printf("[SYNC] failed: %v\n", err)
			return
		}
		if syncConfig.EnableDebug {
			fmt.Printf("[SYNC] synced %d connected peers\n", count)
		}
	}

	// Initial sync so listen mode starts with fresh authorization data.
	syncOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		syncOnce()
	}
}

// listenCmd represents the listen command
var listenCmd = &cobra.Command{
	Use:   "listen",
	Short: "Listens for FlexRadio Discovery Packets on one interface and sends via another",
	Long: `
Listens for FlexRadio Discovery Packets on one interface (e.g. LAN)
and retransmits them via UDP to registered flexclient instances over the VPN.

Flexclients must first send a HELLO control packet:
  HELLO client_ip=<netbird_ip> client_version=<version>

By default, Flextool validates client_ip against the VPN db (flextool.db).
Set CLIENT_AUTH_MODE=registered for deployments where
live HELLO registrations should be trusted without a NetBird user sync.
`,
	Run: func(cmd *cobra.Command, args []string) {
		viperConfig := GetConfig()
		viperConfig.BindPFlag("broadcast", cmd.Flags().Lookup("broadcast"))
		viperConfig.BindPFlag("debug", cmd.Flags().Lookup("debug"))
		viperConfig.BindPFlag("filter", cmd.Flags().Lookup("filter"))
		viperConfig.BindPFlag("port", cmd.Flags().Lookup("port"))
		viperConfig.BindEnv("LISTEN_INTERFACE")
		viperConfig.BindEnv("SEND_INTERFACE")
		viperConfig.BindEnv("DISCOVERY_DELAY_SECONDS")
		viperConfig.BindEnv("SYNC_INTERVAL_SECONDS")
		viperConfig.BindEnv("CLIENT_AUTH_MODE")
		viperConfig.BindEnv("IGNORE_RADIOS")
		viperConfig.BindEnv("NETBIRD_API_TOKEN")
		viperConfig.BindEnv("NETBIRD_API_URL")
		viperConfig.BindEnv("ENABLE_VITA_PROXY")
		viperConfig.BindEnv("VITA_PROXY_PORT")
		viperConfig.BindEnv("PROXY_BASE_PORT")
		viperConfig.BindEnv("PROXY_LAN_SOURCE_IPS")
		viperConfig.BindEnv("MULTI_PROXY")

		viperConfig.AutomaticEnv()

		co, err := ViperValidateListenConfigOptions("listen", viperConfig)
		if err != nil {
			fmt.Printf("INVALID CONFIGURATION ERROR: %s\n", err)
			return
		}

		// Start the flexclient registration + heartbeat server
		if err := utils.StartClientRegistrationServer(&co); err != nil {
			fmt.Println("Error starting client registration server:", err)
			return
		}

		if co.SyncIntervalSeconds > 0 {
			syncConfig, err := ViperValidateSyncConfigOptions(viperConfig)
			if err != nil {
				fmt.Printf("INVALID SYNC CONFIGURATION ERROR: %s\n", err)
				return
			}
			syncConfig.EnableDebug = co.EnableDebug

			go runPeriodicSync(syncConfig, time.Duration(co.SyncIntervalSeconds)*time.Second)
		}

		if co.EnableVitaProxy {
			go func() {
				if err := ListenForVitaStreamPackets(co); err != nil {
					fmt.Println("Error while running VITA proxy listener:", err)
				}
			}()
		}

		// Capture VRT and forward to registered clients
		if err := ListenForPackets(co); err != nil {
			fmt.Println("Error while listening for packets:", err)
		}
	},
}

func init() {
	rootCmd.AddCommand(listenCmd)

	// Local Flags
	listenCmd.Flags().BoolP("broadcast", "b", false, "Forward discovery packets to flexclients")
	listenCmd.Flags().BoolP("debug", "d", false, "Print debug messages")
	listenCmd.Flags().String("filter", "udp and port 4992 and dst host 255.255.255.255", "Berkley packet filter rule")
	listenCmd.Flags().IntVarP(&broadcastPort, "port", "p", 14992, "UDP port flextool listens on for flexclients and sends discovery packets from")
}
