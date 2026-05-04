package flexclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/littleairmada/vrt"
)

const (
	radioModeDirect      = "direct"
	radioModeProxy       = "proxy"
	radioModeOff         = "off"
	defaultProxyBasePort = 30000
	proxyPortSpan        = 20000
)

var (
	ipFieldRe   = regexp.MustCompile(`\bip=\S+`)
	portFieldRe = regexp.MustCompile(`\bport=\d+`)

	clientConfigMu     sync.RWMutex
	clientConfigLoaded bool
	clientConfig       radioModeConfig

	vitaLogMu       sync.Mutex
	vitaLastLogByID = map[string]time.Time{}
	vitaCountByID   = map[string]uint64{}

	identityMu             sync.Mutex
	identityBySerialCached = map[string]discoveryIdentity{} // key: serial(lower)

	localProxyMu       sync.Mutex
	localProxyBySerial = map[string]*localProxyListener{} // key: serial(lower)

	proxySelectMu   sync.Mutex
	proxySelectSent = map[string]time.Time{} // key: routeID|serial

	serialOwnerMu   sync.Mutex
	serialOwnerByID = map[string]serialRouteOwner{} // key: serial(lower)

	rebroadcastMu    sync.Mutex
	rebroadcastConns []*net.UDPConn
	loopbackConn     *net.UDPConn
)

type radioModeConfig struct {
	PerRadioModes map[string]string
	ProxyBasePort int
	IgnoredRoutes map[string]bool
}

type cachedDiscovery struct {
	serial   string
	raw      []byte
	lastSeen time.Time
}

type discoveryIdentity struct {
	serial   string
	nickname string
	callsign string
	model    string
	version  string
}

type localProxyListener struct {
	serial   string
	routeID  string
	serverIP string
	listenIP string
	ln       net.Listener
}

type serialRouteOwner struct {
	routeID  string
	lastSeen time.Time
}

func loadRadioModeConfigFromEnv() radioModeConfig {
	cfg := radioModeConfig{
		PerRadioModes: map[string]string{},
		ProxyBasePort: defaultProxyBasePort,
		IgnoredRoutes: map[string]bool{},
	}

	if raw := strings.TrimSpace(os.Getenv("FLEXCLIENT_PROXY_BASE_PORT")); raw != "" {
		if p, err := strconv.Atoi(raw); err == nil && p >= 1024 && p <= 65535 {
			cfg.ProxyBasePort = p
		}
	}

	// Format: FLEXCLIENT_RADIO_MODES=serial1=proxy,serial2=direct,serial3=off
	// No automatic fallback: if not listed, default is direct.
	if raw := strings.TrimSpace(os.Getenv("FLEXCLIENT_RADIO_MODES")); raw != "" {
		pairs := strings.Split(raw, ",")
		for _, pair := range pairs {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}

			kv := strings.SplitN(pair, "=", 2)
			if len(kv) != 2 {
				continue
			}

			serial := strings.ToLower(strings.TrimSpace(kv[0]))
			mode := strings.ToLower(strings.TrimSpace(kv[1]))
			if serial == "" {
				continue
			}
			if mode != radioModeDirect && mode != radioModeProxy && mode != radioModeOff {
				continue
			}
			cfg.PerRadioModes[serial] = mode
		}
	}

	// Optional: FLEXCLIENT_IGNORE_ROUTES=route-id-1,route-id-2
	if raw := strings.TrimSpace(os.Getenv("FLEXCLIENT_IGNORE_ROUTES")); raw != "" {
		for _, tok := range strings.Split(raw, ",") {
			id := strings.TrimSpace(tok)
			if id == "" {
				continue
			}
			cfg.IgnoredRoutes[id] = true
		}
	}

	return cfg
}

func ensureRadioModeConfigLoaded() {
	clientConfigMu.Lock()
	defer clientConfigMu.Unlock()
	if clientConfigLoaded {
		return
	}
	clientConfig = loadRadioModeConfigFromEnv()
	clientConfigLoaded = true
}

func loadRadioModeConfig() radioModeConfig {
	ensureRadioModeConfigLoaded()

	clientConfigMu.RLock()
	defer clientConfigMu.RUnlock()

	out := radioModeConfig{
		PerRadioModes: map[string]string{},
		ProxyBasePort: clientConfig.ProxyBasePort,
		IgnoredRoutes: map[string]bool{},
	}
	for k, v := range clientConfig.PerRadioModes {
		out.PerRadioModes[k] = v
	}
	for k, v := range clientConfig.IgnoredRoutes {
		out.IgnoredRoutes[k] = v
	}
	return out
}

// SetRadioModeSettings applies explicit per-radio direct/proxy/off mode configuration
// at runtime. Invalid entries are ignored.
func SetRadioModeSettings(proxyBasePort int, perRadioModes map[string]string) {
	ensureRadioModeConfigLoaded()

	if proxyBasePort < 1024 || proxyBasePort > 65535 {
		proxyBasePort = defaultProxyBasePort
	}

	sanitized := map[string]string{}
	for serial, mode := range perRadioModes {
		s := strings.ToLower(strings.TrimSpace(serial))
		m := strings.ToLower(strings.TrimSpace(mode))
		if s == "" {
			continue
		}
		if m != radioModeDirect && m != radioModeProxy && m != radioModeOff {
			continue
		}
		sanitized[s] = m
	}

	clientConfigMu.Lock()
	ignored := map[string]bool{}
	for k, v := range clientConfig.IgnoredRoutes {
		ignored[k] = v
	}
	clientConfig = radioModeConfig{
		PerRadioModes: sanitized,
		ProxyBasePort: proxyBasePort,
		IgnoredRoutes: ignored,
	}
	clientConfigLoaded = true
	clientConfigMu.Unlock()
}

// GetRadioModeSettings returns a snapshot of runtime settings for UI consumption.
func GetRadioModeSettings() (int, map[string]string) {
	cfg := loadRadioModeConfig()
	out := map[string]string{}
	for k, v := range cfg.PerRadioModes {
		out[k] = v
	}
	return cfg.ProxyBasePort, out
}

// GetRadioMode returns the effective mode for a radio serial.
func GetRadioMode(serial string) string {
	return radioModeForSerial(serial)
}

// SetRadioModeForSerial updates a single serial mode at runtime.
func SetRadioModeForSerial(serial, mode string) {
	basePort, modes := GetRadioModeSettings()
	s := strings.ToLower(strings.TrimSpace(serial))
	m := strings.ToLower(strings.TrimSpace(mode))
	if s == "" {
		return
	}
	if m != radioModeDirect && m != radioModeProxy && m != radioModeOff {
		return
	}
	modes[s] = m
	SetRadioModeSettings(basePort, modes)
}

// SetIgnoredRoutes replaces the set of route IDs whose discovery packets should
// be ignored (not rebroadcast locally).
func SetIgnoredRoutes(routeIDs map[string]bool) {
	ensureRadioModeConfigLoaded()

	sanitized := map[string]bool{}
	for routeID, ignored := range routeIDs {
		id := strings.TrimSpace(routeID)
		if id == "" || !ignored {
			continue
		}
		sanitized[id] = true
	}

	clientConfigMu.Lock()
	modes := map[string]string{}
	for k, v := range clientConfig.PerRadioModes {
		modes[k] = v
	}
	clientConfig = radioModeConfig{
		PerRadioModes: modes,
		ProxyBasePort: clientConfig.ProxyBasePort,
		IgnoredRoutes: sanitized,
	}
	clientConfigLoaded = true
	clientConfigMu.Unlock()
}

// SetRouteIgnored toggles route-level ignore.
func SetRouteIgnored(routeID string, ignored bool) {
	cfg := loadRadioModeConfig()
	id := strings.TrimSpace(routeID)
	if id == "" {
		return
	}
	if ignored {
		cfg.IgnoredRoutes[id] = true
	} else {
		delete(cfg.IgnoredRoutes, id)
	}
	SetIgnoredRoutes(cfg.IgnoredRoutes)
}

// IsRouteIgnored reports whether this route is currently ignored.
func IsRouteIgnored(routeID string) bool {
	cfg := loadRadioModeConfig()
	id := strings.TrimSpace(routeID)
	if id == "" {
		return false
	}
	return cfg.IgnoredRoutes[id]
}

// GetIgnoredRoutes returns a snapshot copy of ignored route IDs.
func GetIgnoredRoutes() map[string]bool {
	cfg := loadRadioModeConfig()
	out := map[string]bool{}
	for k, v := range cfg.IgnoredRoutes {
		out[k] = v
	}
	return out
}

func radioModeForSerial(serial string) string {
	cfg := loadRadioModeConfig()
	serial = strings.ToLower(strings.TrimSpace(serial))
	if mode, ok := cfg.PerRadioModes[serial]; ok {
		return mode
	}
	return radioModeDirect
}

func proxyPortForSerial(serial string) int {
	cfg := loadRadioModeConfig()
	sum := crc32.ChecksumIEEE([]byte(strings.ToLower(strings.TrimSpace(serial))))
	return cfg.ProxyBasePort + int(sum%proxyPortSpan)
}

func maybeSendProxySelect(conn *net.UDPConn, routeID, clientIP, serial string) {
	if conn == nil {
		return
	}
	serial = strings.ToLower(strings.TrimSpace(serial))
	if serial == "" {
		return
	}
	clientIP = strings.TrimSpace(clientIP)
	if clientIP == "" {
		return
	}

	key := routeID + "|" + serial
	now := time.Now()

	proxySelectMu.Lock()
	last := proxySelectSent[key]
	if now.Sub(last) < 3*time.Second {
		proxySelectMu.Unlock()
		return
	}
	proxySelectSent[key] = now
	proxySelectMu.Unlock()

	msg := fmt.Sprintf("PROXY_SELECT client_ip=%s serial=%s", clientIP, serial)
	if _, err := conn.Write([]byte(msg)); err != nil {
		log.Printf("flexclient[%s]: failed to send PROXY_SELECT serial=%s: %v", routeID, serial, err)
		return
	}
	log.Printf("flexclient[%s]: sent PROXY_SELECT serial=%s", routeID, serial)
}

func claimSerialOwner(routeID, serial string, now time.Time) bool {
	serial = strings.ToLower(strings.TrimSpace(serial))
	routeID = strings.TrimSpace(routeID)
	if serial == "" || routeID == "" {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}

	serialOwnerMu.Lock()
	defer serialOwnerMu.Unlock()

	current, ok := serialOwnerByID[serial]
	if !ok {
		serialOwnerByID[serial] = serialRouteOwner{routeID: routeID, lastSeen: now}
		return true
	}

	// Owner heartbeat refresh.
	if current.routeID == routeID {
		current.lastSeen = now
		serialOwnerByID[serial] = current
		return true
	}

	// Allow takeover only after owner inactivity window.
	if now.Sub(current.lastSeen) > serialOwnerHold {
		log.Printf("flexclient: serial owner takeover serial=%s %s -> %s", serial, current.routeID, routeID)
		serialOwnerByID[serial] = serialRouteOwner{routeID: routeID, lastSeen: now}
		return true
	}

	return false
}

func releaseRouteSerialOwnership(routeID string) {
	routeID = strings.TrimSpace(routeID)
	if routeID == "" {
		return
	}

	serialOwnerMu.Lock()
	defer serialOwnerMu.Unlock()
	for serial, owner := range serialOwnerByID {
		if owner.routeID == routeID {
			delete(serialOwnerByID, serial)
		}
	}

}

func resetSerialOwnership() {
	serialOwnerMu.Lock()
	defer serialOwnerMu.Unlock()
	serialOwnerByID = map[string]serialRouteOwner{}

	closeAllLocalProxyListeners()
}

func resetIdentityCache() {
	identityMu.Lock()
	defer identityMu.Unlock()
	identityBySerialCached = map[string]discoveryIdentity{}
}

func extractDiscoveryText(raw []byte) (vrt.VRT, string, error) {
	v := vrt.VRT{}
	if err := v.DecodeFromBytes(raw, gopacket.NilDecodeFeedback); err != nil {
		return v, "", err
	}
	text := strings.TrimSpace(string(bytes.TrimRight(v.Payload, "\x00")))
	if !strings.Contains(text, "serial=") || !strings.Contains(text, "ip=") || !strings.Contains(text, "port=") {
		return v, "", fmt.Errorf("not a recognized discovery payload")
	}
	return v, text, nil
}

func fieldValue(payload, key string) string {
	prefix := key + "="
	for _, tok := range strings.Fields(payload) {
		if strings.HasPrefix(tok, prefix) {
			return strings.TrimPrefix(tok, prefix)
		}
	}
	return ""
}

func parseDiscoveryIdentity(payload string) discoveryIdentity {
	return discoveryIdentity{
		serial:   fieldValue(payload, "serial"),
		nickname: fieldValue(payload, "nickname"),
		callsign: fieldValue(payload, "callsign"),
		model:    fieldValue(payload, "model"),
		version:  fieldValue(payload, "version"),
	}
}

func normalizeDiscoveryIdentity(id discoveryIdentity) discoveryIdentity {
	id.serial = strings.ToLower(strings.TrimSpace(id.serial))
	id.nickname = strings.TrimSpace(id.nickname)
	id.callsign = strings.TrimSpace(id.callsign)
	id.model = strings.TrimSpace(id.model)
	id.version = strings.TrimSpace(id.version)
	return id
}

func lockIdentityForSerial(id discoveryIdentity) discoveryIdentity {
	id = normalizeDiscoveryIdentity(id)
	if id.serial == "" {
		return id
	}

	identityMu.Lock()
	defer identityMu.Unlock()

	prev, ok := identityBySerialCached[id.serial]
	if !ok {
		identityBySerialCached[id.serial] = id
		return id
	}

	// Keep stable, first-known identity fields per serial to prevent
	// cross-radio metadata bleed on shared proxy endpoints.
	if prev.nickname != "" {
		id.nickname = prev.nickname
	}
	if prev.callsign != "" {
		id.callsign = prev.callsign
	}
	if prev.model != "" {
		id.model = prev.model
	}
	if prev.version != "" {
		id.version = prev.version
	}

	identityBySerialCached[id.serial] = id
	return id
}

func applyIdentityToPayload(payload string, id discoveryIdentity) string {
	id = normalizeDiscoveryIdentity(id)
	if id.serial == "" {
		return payload
	}
	out := payload
	if id.nickname != "" {
		re := regexp.MustCompile(`\bnickname=\S*`)
		out = re.ReplaceAllString(out, "nickname="+id.nickname)
	}
	if id.callsign != "" {
		re := regexp.MustCompile(`\bcallsign=\S*`)
		out = re.ReplaceAllString(out, "callsign="+id.callsign)
	}
	if id.model != "" {
		re := regexp.MustCompile(`\bmodel=\S*`)
		out = re.ReplaceAllString(out, "model="+id.model)
	}
	if id.version != "" {
		re := regexp.MustCompile(`\bversion=\S*`)
		out = re.ReplaceAllString(out, "version="+id.version)
	}
	return out
}

func loopbackIPForSerial(serial string, salt uint32) string {
	s := strings.ToLower(strings.TrimSpace(serial))
	sum := crc32.ChecksumIEEE([]byte(s))
	v := sum + salt
	// 127/8 loopback; avoid .0/.255 octets.
	o3 := byte((v>>8)%254 + 1)
	o4 := byte(v%254 + 1)
	return fmt.Sprintf("127.77.%d.%d", o3, o4)
}

func ensureLocalProxyListener(serial, routeID, serverIP string) (string, error) {
	serial = strings.ToLower(strings.TrimSpace(serial))
	routeID = strings.TrimSpace(routeID)
	serverIP = strings.TrimSpace(serverIP)
	if serial == "" || routeID == "" || serverIP == "" {
		return "", fmt.Errorf("invalid local proxy listener args")
	}

	localProxyMu.Lock()
	if existing, ok := localProxyBySerial[serial]; ok && existing != nil {
		if existing.routeID == routeID && existing.serverIP == serverIP {
			ip := existing.listenIP
			localProxyMu.Unlock()
			return ip, nil
		}
		if existing.ln != nil {
			_ = existing.ln.Close()
		}
		delete(localProxyBySerial, serial)
	}
	localProxyMu.Unlock()

	var lastErr error
	for attempt := uint32(0); attempt < 64; attempt++ {
		listenIP := loopbackIPForSerial(serial, attempt)
		addr := net.JoinHostPort(listenIP, strconv.Itoa(broadcastPort))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			lastErr = err
			continue
		}

		lp := &localProxyListener{
			serial:   serial,
			routeID:  routeID,
			serverIP: serverIP,
			listenIP: listenIP,
			ln:       ln,
		}
		localProxyMu.Lock()
		localProxyBySerial[serial] = lp
		localProxyMu.Unlock()

		go runLocalProxyAcceptLoop(lp)
		log.Printf("flexclient[%s]: local proxy listener serial=%s on %s -> %s:%d",
			routeID, serial, addr, serverIP, proxyPortForSerial(serial))
		return listenIP, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("failed to bind local proxy listener")
	}
	return "", lastErr
}

func runLocalProxyAcceptLoop(lp *localProxyListener) {
	for {
		clientConn, err := lp.ln.Accept()
		if err != nil {
			return
		}
		go bridgeLocalProxyConn(lp, clientConn)
	}
}

func bridgeLocalProxyConn(lp *localProxyListener, clientConn net.Conn) {
	defer clientConn.Close()

	target := net.JoinHostPort(lp.serverIP, strconv.Itoa(proxyPortForSerial(lp.serial)))
	serverConn, err := net.DialTimeout("tcp", target, 7*time.Second)
	if err != nil {
		log.Printf("flexclient[%s]: local proxy dial failed serial=%s target=%s err=%v",
			lp.routeID, lp.serial, target, err)
		return
	}
	defer serverConn.Close()

	var wg sync.WaitGroup
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = clientConn.Close()
			_ = serverConn.Close()
		})
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(serverConn, clientConn)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, serverConn)
		closeBoth()
	}()
	wg.Wait()
}

func closeAllLocalProxyListeners() {
	localProxyMu.Lock()
	defer localProxyMu.Unlock()
	for serial, lp := range localProxyBySerial {
		if lp != nil && lp.ln != nil {
			_ = lp.ln.Close()
		}
		delete(localProxyBySerial, serial)
	}
}

func reserializeVRTWithPayload(v vrt.VRT, payload string) ([]byte, error) {
	payloadBytes := append([]byte(payload), 0) // NUL-terminated Flex discovery text

	// VRT packet size is measured in 32-bit words. Recompute both payload padding
	// and Header.PacketSize after rewriting the discovery string.
	headerBytes := 4
	if v.Header.Type == vrt.IFDataWithStream || v.Header.Type == vrt.ExtDataWithStream {
		headerBytes += 4
	}
	if v.Header.C {
		headerBytes += 8
	}
	if v.Header.TSI != vrt.TSINone {
		headerBytes += 4
	}
	if v.Header.TSF != vrt.TSFNone {
		headerBytes += 8
	}
	if v.Header.T {
		headerBytes += 4
	}

	totalBytes := headerBytes + len(payloadBytes)
	padding := (4 - (totalBytes % 4)) % 4
	if padding > 0 {
		payloadBytes = append(payloadBytes, make([]byte, padding)...)
		totalBytes += padding
	}

	v.Payload = payloadBytes
	v.Header.PacketSize = uint16(totalBytes / 4)

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: false, ComputeChecksums: false}
	if err := v.SerializeTo(buf, opts); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func rewriteDiscoveryForProxy(raw []byte, serverIP string, proxyPort int) ([]byte, string, error) {
	v, payload, err := extractDiscoveryText(raw)
	if err != nil {
		return nil, "", err
	}
	serial := fieldValue(payload, "serial")
	if serial == "" {
		return nil, "", fmt.Errorf("missing serial field")
	}

	updated := ipFieldRe.ReplaceAllString(payload, "ip="+serverIP)
	updated = portFieldRe.ReplaceAllString(updated, fmt.Sprintf("port=%d", proxyPort))

	origID := parseDiscoveryIdentity(payload)
	newID := parseDiscoveryIdentity(updated)
	if origID.serial == "" || newID.serial == "" || origID.serial != newID.serial {
		return nil, "", fmt.Errorf("proxy rewrite guard: serial changed (%q -> %q)", origID.serial, newID.serial)
	}
	if origID.nickname != newID.nickname {
		return nil, "", fmt.Errorf("proxy rewrite guard: nickname changed for serial=%s (%q -> %q)", origID.serial, origID.nickname, newID.nickname)
	}
	if origID.callsign != newID.callsign {
		return nil, "", fmt.Errorf("proxy rewrite guard: callsign changed for serial=%s (%q -> %q)", origID.serial, origID.callsign, newID.callsign)
	}
	if origID.model != newID.model {
		return nil, "", fmt.Errorf("proxy rewrite guard: model changed for serial=%s (%q -> %q)", origID.serial, origID.model, newID.model)
	}
	if origID.version != newID.version {
		return nil, "", fmt.Errorf("proxy rewrite guard: version changed for serial=%s (%q -> %q)", origID.serial, origID.version, newID.version)
	}

	out, err := reserializeVRTWithPayload(v, updated)
	if err != nil {
		return nil, "", err
	}
	return out, serial, nil
}

// rebroadcastDiscoveryPacket sends the payload to the LAN broadcast address
// on the FlexRadio discovery port (4992).
func rebroadcastDiscoveryPacket(payload []byte) {
	bcastConns, err := getRebroadcastConns()
	if err != nil {
		log.Printf("flexclient: rebroadcast setup error: %v", err)
	} else {
		for _, conn := range bcastConns {
			if conn == nil {
				continue
			}
			if _, err := conn.Write(payload); err != nil {
				log.Printf("flexclient: rebroadcast Write error: %v", err)
				resetRebroadcastConns()
				break
			}
		}
	}

	// Also send to localhost so SmartSDR running on this same machine receives
	// discovery even when the OS does not loop back outbound broadcasts.
	loopConn, err := getLoopbackDiscoveryConn()
	if err != nil {
		log.Printf("flexclient: loopback discovery setup error: %v", err)
		return
	}
	if _, err := loopConn.Write(payload); err != nil {
		log.Printf("flexclient: loopback discovery Write error: %v", err)
		resetRebroadcastConns()
	}
}

func getRebroadcastConns() ([]*net.UDPConn, error) {
	rebroadcastMu.Lock()
	defer rebroadcastMu.Unlock()

	if len(rebroadcastConns) > 0 {
		return rebroadcastConns, nil
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	conns := make([]*net.UDPConn, 0, 4)
	addedTargets := map[string]bool{}

	for _, ifc := range ifaces {
		if (ifc.Flags & net.FlagUp) == 0 {
			continue
		}
		if (ifc.Flags & net.FlagLoopback) != 0 {
			continue
		}
		if (ifc.Flags & net.FlagPointToPoint) != 0 {
			continue
		}
		if isLikelyTunnelInterface(ifc.Name) {
			continue
		}

		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet == nil {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil {
				continue
			}
			if !isRFC1918IPv4(ip4) {
				continue
			}
			bcast := directedBroadcastIPv4(ip4, ipNet.Mask)
			if bcast == nil {
				continue
			}
			local := &net.UDPAddr{IP: ip4, Port: 0}
			destinations := []net.IP{bcast, net.IPv4bcast}
			for _, d := range destinations {
				if d == nil {
					continue
				}
				key := local.IP.String() + "->" + d.String()
				if addedTargets[key] {
					continue
				}
				dest := &net.UDPAddr{IP: d, Port: broadcastPort}
				conn, err := net.DialUDP("udp4", local, dest)
				if err != nil {
					continue
				}
				conns = append(conns, conn)
				addedTargets[key] = true
			}
		}
	}

	// Fallback: if no LAN interfaces were suitable, keep old behavior.
	if len(conns) == 0 {
		dest := &net.UDPAddr{
			IP:   net.IPv4bcast,
			Port: broadcastPort,
		}
		conn, err := net.DialUDP("udp4", nil, dest)
		if err != nil {
			return nil, err
		}
		conns = append(conns, conn)
	}

	rebroadcastConns = conns
	return rebroadcastConns, nil
}

func getLoopbackDiscoveryConn() (*net.UDPConn, error) {
	rebroadcastMu.Lock()
	defer rebroadcastMu.Unlock()

	if loopbackConn != nil {
		return loopbackConn, nil
	}
	dest := &net.UDPAddr{
		IP:   net.ParseIP("127.0.0.1"),
		Port: broadcastPort,
	}
	conn, err := net.DialUDP("udp4", nil, dest)
	if err != nil {
		return nil, err
	}
	loopbackConn = conn
	return loopbackConn, nil
}

func resetRebroadcastConns() {
	rebroadcastMu.Lock()
	defer rebroadcastMu.Unlock()

	for _, conn := range rebroadcastConns {
		if conn != nil {
			_ = conn.Close()
		}
	}
	rebroadcastConns = nil
	if loopbackConn != nil {
		_ = loopbackConn.Close()
		loopbackConn = nil
	}
}

func isLikelyTunnelInterface(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	tunnelHints := []string{
		"netbird", "wt0", "wintun", "wireguard", "wg", "tailscale", "tun", "tap", "utun", "zt",
	}
	for _, h := range tunnelHints {
		if strings.Contains(n, h) {
			return true
		}
	}
	return false
}

func isRFC1918IPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4[0] == 10 {
		return true
	}
	if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
		return true
	}
	if ip4[0] == 192 && ip4[1] == 168 {
		return true
	}
	return false
}

func directedBroadcastIPv4(ip net.IP, mask net.IPMask) net.IP {
	ip4 := ip.To4()
	m4 := mask
	if len(m4) != 4 {
		m4 = mask[len(mask)-4:]
	}
	if ip4 == nil || len(m4) != 4 {
		return nil
	}

	b := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		b[i] = ip4[i] | ^m4[i]
	}
	return b
}

func forwardVitaPacketToLocal(payload []byte, targetIP net.IP) {
	if len(payload) < len(vitaProxyPacketMagic)+2 {
		return
	}
	if !bytes.HasPrefix(payload, []byte(vitaProxyPacketMagic)) {
		return
	}
	offset := len(vitaProxyPacketMagic)
	dstPort := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
	if dstPort <= 0 {
		return
	}
	data := payload[offset+2:]
	if len(data) == 0 {
		return
	}

	destIP := net.ParseIP("127.0.0.1")
	if targetIP != nil {
		if v4 := targetIP.To4(); v4 != nil {
			destIP = v4
		}
	}
	dest := &net.UDPAddr{IP: destIP, Port: dstPort}
	conn, err := net.DialUDP("udp", nil, dest)
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = conn.Write(data)
}

func logVitaPacket(routeID string, dstPort int, payloadLen int, targetIP net.IP) {
	vitaLogMu.Lock()
	defer vitaLogMu.Unlock()

	vitaCountByID[routeID]++
	now := time.Now()
	last := vitaLastLogByID[routeID]
	if now.Sub(last) < 5*time.Second {
		return
	}
	vitaLastLogByID[routeID] = now
	target := "127.0.0.1"
	if targetIP != nil {
		target = targetIP.String()
	}
	log.Printf("flexclient[%s]: received VITA proxy packet -> %s:%d (payload=%d bytes, total=%d)",
		routeID, target, dstPort, payloadLen, vitaCountByID[routeID])
}

func applyDiscoveryModeAndBroadcast(
	conn *net.UDPConn,
	routeID string,
	clientIP string,
	serverIP string,
	serial string,
	payload []byte,
	logRewrite bool,
	discoveryText string,
) {
	mode := radioModeDirect
	if serial != "" {
		mode = radioModeForSerial(serial)
	}

	// No auto-switching:
	// - off mode: do not rebroadcast this radio
	// - direct mode: rebroadcast original discovery
	// - proxy mode: rewrite radio endpoint to server proxy endpoint
	if mode == radioModeOff {
		return
	}

	if serial != "" && mode == radioModeProxy {
		localIP, err := ensureLocalProxyListener(serial, routeID, serverIP)
		if err != nil {
			log.Printf("flexclient[%s]: local proxy listener setup failed serial=%s: %v", routeID, serial, err)
			rebroadcastDiscoveryPacket(payload)
			return
		}

		rewritten, _, err := rewriteDiscoveryForProxy(payload, localIP, broadcastPort)
		if err == nil {
			// Keep PROXY_SELECT for compatibility with shared server listener.
			maybeSendProxySelect(conn, routeID, clientIP, serial)
			if logRewrite && discoveryText != "" {
				origIP := fieldValue(discoveryText, "ip")
				origPort := fieldValue(discoveryText, "port")
				origNick := fieldValue(discoveryText, "nickname")
				origCall := fieldValue(discoveryText, "callsign")
				log.Printf("flexclient[%s]: proxy rewrite serial=%s %s:%s -> %s:%d",
					routeID, serial, origIP, origPort, localIP, broadcastPort)
				log.Printf("flexclient[%s]: identity serial=%s nickname=%q callsign=%q",
					routeID, serial, origNick, origCall)
			}
			rebroadcastDiscoveryPacket(rewritten)
			return
		}
	}

	rebroadcastDiscoveryPacket(payload)
}

func rebroadcastCachedDiscoveries(
	conn *net.UDPConn,
	route Route,
	clientIP string,
	cache map[string]cachedDiscovery,
) {
	if IsRouteIgnored(route.ID) {
		return
	}

	now := time.Now()
	for key, entry := range cache {
		if now.Sub(entry.lastSeen) > discoveryCacheMaxAge {
			delete(cache, key)
			continue
		}
		if !claimSerialOwner(route.ID, entry.serial, now) {
			continue
		}
		applyDiscoveryModeAndBroadcast(conn, route.ID, clientIP, route.IP.String(), entry.serial, entry.raw, false, "")
	}
}

// runForServer is the per-route worker goroutine.
func runForServer(ctx context.Context, route Route, version string) {
	defer releaseRouteSerialOwnership(route.ID)

	serverAddr := &net.UDPAddr{
		IP:   route.IP,
		Port: serverPort,
	}

	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		log.Printf("flexclient[%s]: DialUDP to server %s failed: %v", route.ID, serverAddr.String(), err)
		return
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	clientIP := localAddr.IP.String()
	log.Printf("flexclient[%s]: connected to server %s from local %s (client_ip=%s)",
		route.ID, serverAddr.String(), localAddr.String(), clientIP)

	discoveryCache := map[string]cachedDiscovery{}
	nextCachedRebroadcast := time.Now().Add(discoveryRebroadcastInterval)

	helloPayload := []byte(fmt.Sprintf("HELLO client_ip=%s client_version=%s", clientIP, version))
	if _, err := conn.Write(helloPayload); err != nil {
		log.Printf("flexclient[%s]: initial HELLO send failed: %v", route.ID, err)
	}

	go func() {
		ticker := time.NewTicker(helloInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Printf("flexclient[%s]: HELLO ticker stopping (context cancelled)", route.ID)
				return
			case <-ticker.C:
				if _, err := conn.Write(helloPayload); err != nil {
					log.Printf("flexclient[%s]: HELLO send failed: %v", route.ID, err)
					return
				}
			}
		}
	}()

	buf := make([]byte, 8192)
	for {
		select {
		case <-ctx.Done():
			log.Printf("flexclient[%s]: receive loop stopping (context cancelled)", route.ID)
			return
		default:
		}

		now := time.Now()
		if now.After(nextCachedRebroadcast) {
			rebroadcastCachedDiscoveries(conn, route, clientIP, discoveryCache)
			nextCachedRebroadcast = now.Add(discoveryRebroadcastInterval)
		}

		_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			log.Printf("flexclient[%s]: ReadFromUDP error: %v", route.ID, err)
			time.Sleep(time.Second)
			continue
		}

		payload := buf[:n]

		if bytes.HasPrefix(payload, []byte("HEARTBEAT")) {
			markHeartbeat(route.ID)
			continue
		}

		if bytes.HasPrefix(payload, []byte(vitaProxyPacketMagic)) {
			if len(payload) >= len(vitaProxyPacketMagic)+2 {
				offset := len(vitaProxyPacketMagic)
				dstPort := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
				dataLen := len(payload) - (offset + 2)
				if dataLen < 0 {
					dataLen = 0
				}
				logVitaPacket(route.ID, dstPort, dataLen, localAddr.IP)
			}
			forwardVitaPacketToLocal(payload, localAddr.IP)
			continue
		}

		// Only rebroadcast validated discovery frames to SmartSDR. Unknown UDP
		// payloads on the control channel can destabilize radio discovery lists.
		_, discoveryText, err := extractDiscoveryText(payload)
		if err != nil {
			continue
		}

		serial := strings.ToLower(strings.TrimSpace(fieldValue(discoveryText, "serial")))
		if serial == "" {
			continue
		}

		now = time.Now()
		discoveryCache[serial] = cachedDiscovery{
			serial:   serial,
			raw:      append([]byte(nil), payload...),
			lastSeen: now,
		}

		markDiscovery(route.ID, serial)
		if IsRouteIgnored(route.ID) {
			continue
		}
		if !claimSerialOwner(route.ID, serial, now) {
			continue
		}
		applyDiscoveryModeAndBroadcast(conn, route.ID, clientIP, route.IP.String(), serial, payload, true, discoveryText)
	}
}
