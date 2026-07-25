package flexclient

import (
	"bufio"
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
	radioModeDirect       = "direct"
	radioModeProxy        = "proxy"
	radioModeOff          = "off"
	defaultProxyBasePort  = 30000
	proxyPortSpan         = 20000
	udpSocketBufferSize   = 4 * 1024 * 1024
	vitaForwardQueueSize  = 4096
	vitaForwardPrimeWait  = 80 * time.Millisecond
	vitaForwardIdleReset  = 200 * time.Millisecond
	directAssistQueueSize = 1024
	directAssistPrimeWait = 20 * time.Millisecond
	directAssistIdleReset = 150 * time.Millisecond
	directPunchVITAPort   = 4991
	directPunchInterval   = 10 * time.Second
)

var directAssistRadioUDPPorts = []int{4991, 4993}

const directAssistRadioTXPort = 4991

var (
	ipFieldRe   = regexp.MustCompile(`\bip=\S+`)
	portFieldRe = regexp.MustCompile(`\bport=\d+`)

	clientConfigMu     sync.RWMutex
	clientConfigLoaded bool
	clientConfig       radioModeConfig

	vitaLogMu            sync.Mutex
	vitaLastLogByID      = map[string]time.Time{}
	vitaLastPacketByID   = map[string]time.Time{}
	vitaLastLogCountByID = map[string]uint64{}
	vitaCountByID        = map[string]uint64{}
	vitaBytesByID        = map[string]uint64{}
	vitaLastLogBytesByID = map[string]uint64{}
	vitaMaxGapByID       = map[string]time.Duration{}
	vitaForwardLogMu     sync.Mutex
	vitaForwardLastLog   = map[string]time.Time{}
	vitaForwardLastWrite = map[string]time.Time{}
	vitaForwardLastCount = map[string]uint64{}
	vitaForwardCount     = map[string]uint64{}
	vitaForwardMaxGap    = map[string]time.Duration{}
	vitaForwardMu        sync.Mutex
	vitaForwardConn      = map[string]*net.UDPConn{}
	vitaForwardQueue     = map[string]chan vitaForwardFrame{}
	discoveryLogMu       sync.Mutex
	discoveryLastLog     = map[string]time.Time{}

	identityMu             sync.Mutex
	identityBySerialCached = map[string]discoveryIdentity{} // key: serial(lower)

	localDirectAssistMu       sync.Mutex
	localDirectAssistBySerial = map[string]*localDirectAssistListener{} // key: serial(lower)

	serialOwnerMu   sync.Mutex
	serialOwnerByID = map[string]serialRouteOwner{} // key: serial(lower)

	directPunchMu    sync.Mutex
	directPunchConns = map[string]*directPunchConn{}

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
	radioIP  string
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

type localDirectAssistListener struct {
	serial    string
	routeID   string
	clientIP  string
	radioIP   string
	radioPort int
	listenIP  string
	ln        net.Listener
	udpConns  []*net.UDPConn

	mu             sync.RWMutex
	vitaBySmartUDP map[int]*directAssistVITASession

	closeOnce sync.Once
	done      chan struct{}
}

type directAssistVITASession struct {
	smartSDRPort int
	conn         *net.UDPConn
	vitaPort     int
	packets      uint64
	lastLog      time.Time
	lastBytes    uint64
}

type serialRouteOwner struct {
	routeID  string
	lastSeen time.Time
	score    int
}

type vitaForwardFrame struct {
	destIP  net.IP
	dstPort int
	data    []byte
}

type directPunchConn struct {
	conn     *net.UDPConn
	lastSent time.Time
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
			mode := normalizeRadioMode(strings.TrimSpace(kv[1]))
			if serial == "" {
				continue
			}
			if mode == "" {
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

// SetRadioModeSettings applies explicit per-radio on/off mode configuration
// at runtime. Invalid entries are ignored.
func SetRadioModeSettings(proxyBasePort int, perRadioModes map[string]string) {
	ensureRadioModeConfigLoaded()

	if proxyBasePort < 1024 || proxyBasePort > 65535 {
		proxyBasePort = defaultProxyBasePort
	}

	sanitized := map[string]string{}
	for serial, mode := range perRadioModes {
		s := strings.ToLower(strings.TrimSpace(serial))
		m := normalizeRadioMode(mode)
		if s == "" {
			continue
		}
		if m == "" {
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
	m := normalizeRadioMode(mode)
	if s == "" {
		return
	}
	if m == "" {
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
		if normalized := normalizeRadioMode(mode); normalized != "" {
			return normalized
		}
	}
	return radioModeDirect
}

func normalizeRadioMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "on", "enabled", "enable", "true", "yes", radioModeDirect, radioModeProxy:
		return radioModeDirect
	case radioModeOff, "disabled", "disable", "false", "no":
		return radioModeOff
	default:
		return ""
	}
}

func directPunchEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("FLEXCLIENT_DIRECT_PUNCH")))
	switch raw {
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return true
	}
}

func directPunchPort() int {
	raw := strings.TrimSpace(os.Getenv("FLEXCLIENT_DIRECT_PUNCH_PORT"))
	if raw == "" {
		return directPunchVITAPort
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port <= 0 || port >= 65536 {
		return directPunchVITAPort
	}
	return port
}

func directAssistEnabled() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("FLEXCLIENT_DIRECT_ASSIST")))
	switch raw {
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return true
	}
}

func maybeSendDirectRadioPunch(routeID, serial, clientIP string, payload []byte, discoveryText string) {
	if !directPunchEnabled() {
		return
	}

	serial = strings.ToLower(strings.TrimSpace(serial))
	clientIP = strings.TrimSpace(clientIP)
	if routeID == "" || serial == "" || clientIP == "" {
		return
	}

	if strings.TrimSpace(discoveryText) == "" && len(payload) > 0 {
		_, text, err := extractDiscoveryText(payload)
		if err != nil {
			return
		}
		discoveryText = text
	}

	radioIP := net.ParseIP(strings.TrimSpace(fieldValue(discoveryText, "ip"))).To4()
	if radioIP == nil {
		return
	}

	localIP := net.ParseIP(clientIP).To4()
	port := directPunchPort()
	dest := &net.UDPAddr{IP: radioIP, Port: port}
	localKey := clientIP
	if localIP == nil {
		localKey = ""
	}
	key := routeID + "|" + serial + "|" + localKey + "->" + dest.String()

	directPunchMu.Lock()
	entry := directPunchConns[key]
	if entry != nil && time.Since(entry.lastSent) < directPunchInterval {
		directPunchMu.Unlock()
		return
	}
	if entry == nil || entry.conn == nil {
		conn, err := dialDirectPunchUDP(localIP, dest)
		if err != nil {
			directPunchMu.Unlock()
			log.Printf("flexclient[%s]: direct UDP punch setup failed serial=%s radio=%s err=%v",
				routeID, serial, dest.String(), err)
			return
		}
		entry = &directPunchConn{conn: conn}
		directPunchConns[key] = entry
	}
	entry.lastSent = time.Now()
	conn := entry.conn
	directPunchMu.Unlock()

	if _, err := conn.Write([]byte("FRNT_DIRECT_PUNCH")); err != nil {
		directPunchMu.Lock()
		if current := directPunchConns[key]; current == entry {
			delete(directPunchConns, key)
		}
		directPunchMu.Unlock()
		_ = conn.Close()
		log.Printf("flexclient[%s]: direct UDP punch failed serial=%s radio=%s err=%v",
			routeID, serial, dest.String(), err)
		return
	}

	log.Printf("flexclient[%s]: direct UDP punch serial=%s %s -> %s",
		routeID, serial, clientIP, dest.String())
}

func dialDirectPunchUDP(localIP net.IP, dest *net.UDPAddr) (*net.UDPConn, error) {
	if dest == nil || dest.IP == nil || dest.Port <= 0 {
		return nil, fmt.Errorf("invalid direct punch destination")
	}
	if localIP != nil {
		conn, err := net.DialUDP("udp4", &net.UDPAddr{IP: localIP, Port: 0}, dest)
		if err == nil {
			_ = conn.SetWriteBuffer(udpSocketBufferSize)
			return conn, nil
		}
	}
	conn, err := net.DialUDP("udp4", nil, dest)
	if err != nil {
		return nil, err
	}
	_ = conn.SetWriteBuffer(udpSocketBufferSize)
	return conn, nil
}

func routeRadioAffinityScore(routeIP net.IP, radioIP string) int {
	rv4 := routeIP.To4()
	radioV4 := net.ParseIP(strings.TrimSpace(radioIP)).To4()
	if rv4 == nil || radioV4 == nil {
		return 0
	}
	if rv4.Equal(radioV4) {
		return 100
	}
	if rv4[0] == radioV4[0] && rv4[1] == radioV4[1] && rv4[2] == radioV4[2] {
		return 90
	}
	if rv4[0] == radioV4[0] && rv4[1] == radioV4[1] {
		return 40
	}
	return 0
}

func claimSerialOwner(routeID, serial string, now time.Time) bool {
	return claimSerialOwnerWithScore(routeID, serial, now, 0)
}

func claimSerialOwnerWithScore(routeID, serial string, now time.Time, score int) bool {
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
		serialOwnerByID[serial] = serialRouteOwner{routeID: routeID, lastSeen: now, score: score}
		return true
	}

	// Owner heartbeat refresh.
	if current.routeID == routeID {
		current.lastSeen = now
		if score > current.score {
			current.score = score
		}
		serialOwnerByID[serial] = current
		return true
	}

	if score > current.score {
		log.Printf("flexclient: serial owner affinity takeover serial=%s %s(score=%d) -> %s(score=%d)",
			serial, current.routeID, current.score, routeID, score)
		serialOwnerByID[serial] = serialRouteOwner{routeID: routeID, lastSeen: now, score: score}
		removeRadioFromOtherRoutes(routeID, serial)
		return true
	}

	// Allow takeover only after owner inactivity window.
	if now.Sub(current.lastSeen) > serialOwnerHold {
		log.Printf("flexclient: serial owner takeover serial=%s %s -> %s", serial, current.routeID, routeID)
		serialOwnerByID[serial] = serialRouteOwner{routeID: routeID, lastSeen: now, score: score}
		removeRadioFromOtherRoutes(routeID, serial)
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

	closeAllLocalDirectAssistListeners()
	resetVitaForwardConns()
	resetDirectPunchConns()
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

func isModernDiscoveryPayload(payload string) bool {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return false
	}
	// Prefer the modern protocol/status payload to avoid nickname/callsign
	// flip-flopping from mixed legacy discovery formats.
	return strings.Contains(payload, "discovery_protocol_version=")
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

func ensureLocalDirectAssistListener(serial, routeID, clientIP, radioIP string, radioPort int) (string, error) {
	serial = strings.ToLower(strings.TrimSpace(serial))
	routeID = strings.TrimSpace(routeID)
	clientIP = strings.TrimSpace(clientIP)
	radioIP = strings.TrimSpace(radioIP)
	if radioPort <= 0 || radioPort >= 65536 {
		radioPort = broadcastPort
	}
	if serial == "" || routeID == "" || clientIP == "" || net.ParseIP(radioIP) == nil {
		return "", fmt.Errorf("invalid local direct assist args")
	}

	localDirectAssistMu.Lock()
	if existing, ok := localDirectAssistBySerial[serial]; ok && existing != nil {
		if existing.routeID == routeID && existing.clientIP == clientIP && existing.radioIP == radioIP && existing.radioPort == radioPort {
			ip := existing.listenIP
			localDirectAssistMu.Unlock()
			return ip, nil
		}
		closeLocalDirectAssist(existing)
		delete(localDirectAssistBySerial, serial)
	}
	localDirectAssistMu.Unlock()

	var lastErr error
	for attempt := uint32(1024); attempt < 1088; attempt++ {
		listenIP := loopbackIPForSerial(serial, attempt)
		addr := net.JoinHostPort(listenIP, strconv.Itoa(broadcastPort))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			lastErr = err
			continue
		}
		udpConns, err := listenDirectAssistUDPConns(listenIP)
		if err != nil {
			_ = ln.Close()
			lastErr = err
			continue
		}

		da := &localDirectAssistListener{
			serial:         serial,
			routeID:        routeID,
			clientIP:       clientIP,
			radioIP:        radioIP,
			radioPort:      radioPort,
			listenIP:       listenIP,
			ln:             ln,
			udpConns:       udpConns,
			vitaBySmartUDP: map[int]*directAssistVITASession{},
			done:           make(chan struct{}),
		}

		localDirectAssistMu.Lock()
		localDirectAssistBySerial[serial] = da
		localDirectAssistMu.Unlock()

		go runLocalDirectAssistAcceptLoop(da)
		for _, udpConn := range udpConns {
			go runLocalDirectAssistUDPForwardLoop(da, udpConn)
		}
		log.Printf("flexclient[%s]: local direct assist serial=%s on %s -> %s:%d",
			routeID, serial, addr, radioIP, radioPort)
		return listenIP, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("failed to bind local direct assist listener")
	}
	return "", lastErr
}

func listenDirectAssistUDPConns(listenIP string) ([]*net.UDPConn, error) {
	ports := []int{directAssistRadioTXPort, broadcastPort}
	seen := map[int]bool{}
	var conns []*net.UDPConn
	for _, port := range ports {
		if port <= 0 || port >= 65536 || seen[port] {
			continue
		}
		seen[port] = true
		udpAddr := &net.UDPAddr{IP: net.ParseIP(listenIP), Port: port}
		udpConn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			for _, conn := range conns {
				_ = conn.Close()
			}
			return nil, err
		}
		_ = udpConn.SetReadBuffer(udpSocketBufferSize)
		_ = udpConn.SetWriteBuffer(udpSocketBufferSize)
		conns = append(conns, udpConn)
	}
	if len(conns) == 0 {
		return nil, fmt.Errorf("failed to bind local direct assist UDP listeners")
	}
	return conns, nil
}

func runLocalDirectAssistAcceptLoop(da *localDirectAssistListener) {
	for {
		clientConn, err := da.ln.Accept()
		if err != nil {
			return
		}
		go bridgeLocalDirectAssistConn(da, clientConn)
	}
}

func runLocalDirectAssistUDPForwardLoop(da *localDirectAssistListener, udpConn *net.UDPConn) {
	if da == nil || udpConn == nil {
		return
	}
	radioIP := net.ParseIP(da.radioIP)
	if radioIP == nil {
		return
	}
	dest := &net.UDPAddr{IP: radioIP, Port: directAssistRadioTXPort}
	var localAddr *net.UDPAddr
	if clientIP := net.ParseIP(da.clientIP).To4(); clientIP != nil {
		localAddr = &net.UDPAddr{IP: clientIP, Port: 0}
	}
	txConn, err := net.DialUDP("udp4", localAddr, dest)
	if err != nil {
		log.Printf("flexclient[%s]: local direct assist TX dial failed serial=%s dest=%s err=%v",
			da.routeID, da.serial, dest.String(), err)
		return
	}
	defer txConn.Close()
	_ = txConn.SetWriteBuffer(udpSocketBufferSize)

	buf := make([]byte, 8192)
	for {
		n, addr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n <= 0 {
			continue
		}
		if _, err := txConn.Write(buf[:n]); err != nil {
			log.Printf("flexclient[%s]: local direct assist TX write failed serial=%s dest=%s err=%v",
				da.routeID, da.serial, dest.String(), err)
			return
		}
		logDirectAssistTX(da, addr, n, dest)
	}
}

var directAssistTXLogMu sync.Mutex
var directAssistTXLastLog = map[string]time.Time{}
var directAssistTXPackets = map[string]uint64{}
var directAssistTXBytes = map[string]uint64{}

func logDirectAssistTX(da *localDirectAssistListener, src *net.UDPAddr, n int, dest *net.UDPAddr) {
	if da == nil || src == nil || dest == nil {
		return
	}
	key := da.serial + "|" + src.String()
	now := time.Now()

	directAssistTXLogMu.Lock()
	defer directAssistTXLogMu.Unlock()
	directAssistTXPackets[key]++
	directAssistTXBytes[key] += uint64(n)
	if now.Sub(directAssistTXLastLog[key]) < 5*time.Second {
		return
	}
	directAssistTXLastLog[key] = now
	log.Printf("flexclient[%s]: local direct assist TX serial=%s local=%s packets=%d bytes=%d -> radio=%s",
		da.routeID, da.serial, src.String(), directAssistTXPackets[key], directAssistTXBytes[key], dest.String())
	directAssistTXPackets[key] = 0
	directAssistTXBytes[key] = 0
}

func bridgeLocalDirectAssistConn(da *localDirectAssistListener, clientConn net.Conn) {
	defer clientConn.Close()

	target := net.JoinHostPort(da.radioIP, strconv.Itoa(da.radioPort))
	radioConn, err := net.DialTimeout("tcp", target, 7*time.Second)
	if err != nil {
		log.Printf("flexclient[%s]: local direct assist dial failed serial=%s target=%s err=%v",
			da.routeID, da.serial, target, err)
		return
	}
	defer radioConn.Close()

	var wg sync.WaitGroup
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = clientConn.Close()
			_ = radioConn.Close()
		})
	}

	wg.Add(2)
	go func() {
		defer wg.Done()
		copySmartSDRToRadioWithDirectAssist(da, clientConn, radioConn)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(clientConn, radioConn)
		closeBoth()
	}()
	wg.Wait()
}

func copySmartSDRToRadioWithDirectAssist(da *localDirectAssistListener, src net.Conn, dst net.Conn) {
	reader := bufio.NewReader(src)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			out := rewriteClientUDPPortForDirectAssist(da, line)
			if len(out) > 0 {
				if _, writeErr := dst.Write(out); writeErr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func rewriteClientUDPPortForDirectAssist(da *localDirectAssistListener, line []byte) []byte {
	if da == nil || len(line) == 0 {
		return line
	}

	lower := strings.ToLower(string(line))
	const needle = "client udpport "
	idx := strings.Index(lower, needle)
	if idx < 0 {
		return line
	}

	start := idx + len(needle)
	end := start
	for end < len(lower) && lower[end] >= '0' && lower[end] <= '9' {
		end++
	}
	if end == start {
		return line
	}

	origPort, err := strconv.Atoi(lower[start:end])
	if err != nil || origPort <= 0 || origPort >= 65536 {
		return line
	}

	vitaPort, err := ensureDirectAssistVITAConn(da, origPort)
	if err != nil {
		log.Printf("flexclient[%s]: local direct assist VITA setup failed serial=%s smart_port=%d err=%v",
			da.routeID, da.serial, origPort, err)
		return line
	}

	replacement := []byte(strconv.Itoa(vitaPort))
	out := make([]byte, 0, len(line)+len(replacement)-(end-start))
	out = append(out, line[:start]...)
	out = append(out, replacement...)
	out = append(out, line[end:]...)
	log.Printf("flexclient[%s]: local direct assist serial=%s client udpport %d -> %d",
		da.routeID, da.serial, origPort, vitaPort)
	return out
}

func ensureDirectAssistVITAConn(da *localDirectAssistListener, smartSDRPort int) (int, error) {
	if da == nil || smartSDRPort <= 0 || smartSDRPort >= 65536 {
		return 0, fmt.Errorf("invalid SmartSDR UDP port")
	}

	da.mu.Lock()
	defer da.mu.Unlock()
	if da.vitaBySmartUDP == nil {
		da.vitaBySmartUDP = map[int]*directAssistVITASession{}
	}
	if session := da.vitaBySmartUDP[smartSDRPort]; session != nil && session.conn != nil && session.vitaPort > 0 {
		return session.vitaPort, nil
	}

	localIP := net.ParseIP(da.clientIP).To4()
	localAddr := &net.UDPAddr{IP: localIP, Port: 0}
	conn, err := net.ListenUDP("udp4", localAddr)
	if err != nil && localIP != nil {
		conn, err = net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
	}
	if err != nil {
		return 0, err
	}
	_ = conn.SetReadBuffer(udpSocketBufferSize)
	_ = conn.SetWriteBuffer(udpSocketBufferSize)

	localUDP, _ := conn.LocalAddr().(*net.UDPAddr)
	if localUDP == nil || localUDP.Port <= 0 {
		_ = conn.Close()
		return 0, fmt.Errorf("failed to determine local VITA UDP port")
	}

	session := &directAssistVITASession{
		smartSDRPort: smartSDRPort,
		conn:         conn,
		vitaPort:     localUDP.Port,
	}
	da.vitaBySmartUDP[smartSDRPort] = session
	go runDirectAssistVITAForwardLoop(da, session)
	go runDirectAssistVITAKeepaliveLoop(da, session)

	log.Printf("flexclient[%s]: local direct assist VITA serial=%s listen=%s smart_udp=127.0.0.1:%d radio=%s:%v",
		da.routeID, da.serial, localUDP.String(), smartSDRPort, da.radioIP, directAssistRadioUDPPorts)
	return session.vitaPort, nil
}

func runDirectAssistVITAKeepaliveLoop(da *localDirectAssistListener, session *directAssistVITASession) {
	if session == nil || session.conn == nil {
		return
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	radioIP := net.ParseIP(da.radioIP)
	if radioIP == nil {
		return
	}
	payload := []byte("FRNT_DIRECT_ASSIST")

	for {
		select {
		case <-da.done:
			return
		case <-ticker.C:
			for _, port := range directAssistRadioUDPPorts {
				if port <= 0 || port >= 65536 {
					continue
				}
				_, _ = session.conn.WriteToUDP(payload, &net.UDPAddr{IP: radioIP, Port: port})
			}
		}
	}
}

func runDirectAssistVITAForwardLoop(da *localDirectAssistListener, session *directAssistVITASession) {
	if session == nil || session.conn == nil {
		return
	}
	dest := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: session.smartSDRPort}
	out, err := net.DialUDP("udp4", nil, dest)
	if err != nil {
		log.Printf("flexclient[%s]: local direct assist local UDP dial failed serial=%s dest=%s err=%v",
			da.routeID, da.serial, dest.String(), err)
		return
	}
	defer out.Close()
	_ = out.SetWriteBuffer(udpSocketBufferSize)

	queue := make(chan []byte, directAssistQueueSize)
	go runDirectAssistVITAWriter(da, session, out, queue)

	buf := make([]byte, 8192)
	for {
		n, addr, err := session.conn.ReadFromUDP(buf)
		if err != nil {
			close(queue)
			return
		}
		if n <= 0 {
			continue
		}
		if addr != nil && addr.IP != nil {
			radioIP := net.ParseIP(da.radioIP)
			if radioIP != nil && !addr.IP.Equal(radioIP) {
				continue
			}
		}
		frame := append([]byte(nil), buf[:n]...)
		select {
		case queue <- frame:
		default:
			// Keep current display/audio data when the tunnel delivers a burst.
			select {
			case <-queue:
			default:
			}
			select {
			case queue <- frame:
			default:
			}
		}
	}
}

func runDirectAssistVITAWriter(da *localDirectAssistListener, session *directAssistVITASession, out *net.UDPConn, queue <-chan []byte) {
	var lastWrite time.Time
	for frame := range queue {
		now := time.Now()
		if lastWrite.IsZero() || now.Sub(lastWrite) > directAssistIdleReset {
			select {
			case <-da.done:
				return
			case <-time.After(directAssistPrimeWait):
			}
		}

		if _, err := out.Write(frame); err != nil {
			return
		}
		lastWrite = time.Now()
		logDirectAssistVITA(da, session, len(frame))

		if sleep := directAssistPaceInterval(len(queue)); sleep > 0 {
			select {
			case <-da.done:
				return
			case <-time.After(sleep):
			}
		}
	}
}

func directAssistPaceInterval(queued int) time.Duration {
	switch {
	case queued > 200:
		return time.Millisecond
	case queued > 50:
		return 2 * time.Millisecond
	case queued > 5:
		return 4 * time.Millisecond
	default:
		return 0
	}
}

func logDirectAssistVITA(da *localDirectAssistListener, session *directAssistVITASession, n int) {
	if da == nil || session == nil {
		return
	}
	da.mu.Lock()
	defer da.mu.Unlock()
	now := time.Now()
	session.packets++
	session.lastBytes += uint64(n)
	if now.Sub(session.lastLog) < 5*time.Second {
		return
	}
	elapsed := now.Sub(session.lastLog).Seconds()
	if session.lastLog.IsZero() || elapsed <= 0 {
		elapsed = 5
	}
	pps := float64(session.packets) / elapsed
	log.Printf("flexclient[%s]: local direct assist VITA serial=%s tunnel_udp=%d packets=%d pps=%.1f bytes=%d -> 127.0.0.1:%d",
		da.routeID, da.serial, session.vitaPort, session.packets, pps, session.lastBytes, session.smartSDRPort)
	session.packets = 0
	session.lastBytes = 0
	session.lastLog = now
}

func closeAllLocalDirectAssistListeners() {
	localDirectAssistMu.Lock()
	defer localDirectAssistMu.Unlock()
	for serial, da := range localDirectAssistBySerial {
		closeLocalDirectAssist(da)
		delete(localDirectAssistBySerial, serial)
	}
}

func closeLocalDirectAssist(da *localDirectAssistListener) {
	if da == nil {
		return
	}
	da.closeOnce.Do(func() {
		close(da.done)
		if da.ln != nil {
			_ = da.ln.Close()
		}
		da.mu.Lock()
		for port, session := range da.vitaBySmartUDP {
			if session != nil && session.conn != nil {
				_ = session.conn.Close()
			}
			delete(da.vitaBySmartUDP, port)
		}
		for _, conn := range da.udpConns {
			if conn != nil {
				_ = conn.Close()
			}
		}
		da.mu.Unlock()
	})
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

func resetVitaForwardConns() {
	vitaForwardMu.Lock()
	defer vitaForwardMu.Unlock()
	for key, conn := range vitaForwardConn {
		if conn != nil {
			_ = conn.Close()
		}
		delete(vitaForwardConn, key)
	}
}

func resetDirectPunchConns() {
	directPunchMu.Lock()
	defer directPunchMu.Unlock()
	for key, entry := range directPunchConns {
		if entry != nil && entry.conn != nil {
			_ = entry.conn.Close()
		}
		delete(directPunchConns, key)
	}
}

func isLikelyTunnelInterface(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	tunnelHints := []string{
		"netbird", "wt0", "wintun", "wireguard", "wg", "tun", "tap", "utun", "zt",
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
	frame, key, ok := parseVitaForwardFrame(payload, targetIP)
	if !ok {
		return
	}

	ch := getVitaForwardQueue(key, frame.destIP, frame.dstPort)
	select {
	case ch <- frame:
		return
	default:
		// Prefer current audio over stale queued audio when Windows or the tunnel
		// delivers VITA in bursts.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- frame:
		default:
		}
	}
}

func parseVitaForwardFrame(payload []byte, targetIP net.IP) (vitaForwardFrame, string, bool) {
	var frame vitaForwardFrame
	if len(payload) < len(vitaProxyPacketMagic)+2 {
		return frame, "", false
	}
	if !bytes.HasPrefix(payload, []byte(vitaProxyPacketMagic)) {
		return frame, "", false
	}
	offset := len(vitaProxyPacketMagic)
	dstPort := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
	if dstPort <= 0 {
		return frame, "", false
	}
	data := payload[offset+2:]
	if len(data) == 0 {
		return frame, "", false
	}
	destIP := net.ParseIP("127.0.0.1")
	if targetIP != nil {
		if v4 := targetIP.To4(); v4 != nil {
			destIP = v4
		}
	}
	frame = vitaForwardFrame{
		destIP:  append(net.IP(nil), destIP...),
		dstPort: dstPort,
		data:    append([]byte(nil), data...),
	}
	key := destIP.String() + ":" + strconv.Itoa(dstPort)
	return frame, key, true
}

func getVitaForwardQueue(key string, destIP net.IP, dstPort int) chan vitaForwardFrame {
	vitaForwardMu.Lock()
	defer vitaForwardMu.Unlock()
	if ch := vitaForwardQueue[key]; ch != nil {
		return ch
	}
	ch := make(chan vitaForwardFrame, vitaForwardQueueSize)
	vitaForwardQueue[key] = ch
	go runVitaForwardWorker(key, append(net.IP(nil), destIP...), dstPort, ch)
	return ch
}

func runVitaForwardWorker(key string, destIP net.IP, dstPort int, ch <-chan vitaForwardFrame) {
	var lastFrame time.Time
	for frame := range ch {
		now := time.Now()
		if lastFrame.IsZero() || now.Sub(lastFrame) > vitaForwardIdleReset {
			time.Sleep(vitaForwardPrimeWait)
		}
		lastFrame = time.Now()

		conn, connKey, err := getVitaForwardConn(destIP, dstPort)
		if err != nil {
			continue
		}
		if _, err := conn.Write(frame.data); err != nil {
			removeVitaForwardConn(connKey, conn)
		} else {
			logVitaForwardedPacket(key, len(frame.data), len(ch))
		}

		sleep := vitaForwardPaceInterval(len(ch))
		if sleep > 0 {
			time.Sleep(sleep)
		}
	}
}

func logVitaForwardedPacket(key string, payloadLen int, queued int) {
	vitaForwardLogMu.Lock()
	defer vitaForwardLogMu.Unlock()

	now := time.Now()
	vitaForwardCount[key]++
	if lastWrite := vitaForwardLastWrite[key]; !lastWrite.IsZero() {
		if gap := now.Sub(lastWrite); gap > vitaForwardMaxGap[key] {
			vitaForwardMaxGap[key] = gap
		}
	}
	vitaForwardLastWrite[key] = now

	lastLog := vitaForwardLastLog[key]
	if now.Sub(lastLog) < 5*time.Second {
		return
	}
	elapsed := now.Sub(lastLog).Seconds()
	if lastLog.IsZero() || elapsed <= 0 {
		elapsed = 5
	}
	total := vitaForwardCount[key]
	intervalPackets := total - vitaForwardLastCount[key]
	pps := float64(intervalPackets) / elapsed
	maxGap := vitaForwardMaxGap[key]

	vitaForwardLastLog[key] = now
	vitaForwardLastCount[key] = total
	vitaForwardMaxGap[key] = 0

	log.Printf("flexclient: forwarded VITA local -> %s (payload=%d bytes, total=%d, pps=%.1f, max_gap=%s, queued=%d)",
		key, payloadLen, total, pps, maxGap.Truncate(time.Millisecond), queued)
}

func vitaForwardPaceInterval(queued int) time.Duration {
	switch {
	case queued > 300:
		return time.Millisecond
	case queued > 100:
		return 2 * time.Millisecond
	case queued > 20:
		return 4 * time.Millisecond
	default:
		return 4900 * time.Microsecond
	}
}

func removeVitaForwardConn(key string, conn *net.UDPConn) {
	if conn == nil {
		return
	}
	vitaForwardMu.Lock()
	if current := vitaForwardConn[key]; current == conn {
		delete(vitaForwardConn, key)
	}
	vitaForwardMu.Unlock()
	_ = conn.Close()
}

func getVitaForwardConn(destIP net.IP, dstPort int) (*net.UDPConn, string, error) {
	if destIP == nil || dstPort <= 0 || dstPort >= 65536 {
		return nil, "", fmt.Errorf("invalid VITA forward destination")
	}
	key := destIP.String() + ":" + strconv.Itoa(dstPort)

	vitaForwardMu.Lock()
	defer vitaForwardMu.Unlock()
	if conn := vitaForwardConn[key]; conn != nil {
		return conn, key, nil
	}

	dest := &net.UDPAddr{IP: destIP, Port: dstPort}
	conn, err := net.DialUDP("udp", nil, dest)
	if err != nil {
		return nil, key, err
	}
	_ = conn.SetWriteBuffer(udpSocketBufferSize)
	vitaForwardConn[key] = conn
	return conn, key, nil
}

func logVitaPacket(routeID string, dstPort int, payloadLen int, targetIP net.IP) {
	vitaLogMu.Lock()
	defer vitaLogMu.Unlock()

	now := time.Now()
	vitaCountByID[routeID]++
	vitaBytesByID[routeID] += uint64(payloadLen)
	if lastPacket := vitaLastPacketByID[routeID]; !lastPacket.IsZero() {
		if gap := now.Sub(lastPacket); gap > vitaMaxGapByID[routeID] {
			vitaMaxGapByID[routeID] = gap
		}
	}
	vitaLastPacketByID[routeID] = now

	last := vitaLastLogByID[routeID]
	if now.Sub(last) < 5*time.Second {
		return
	}
	elapsed := now.Sub(last).Seconds()
	if last.IsZero() || elapsed <= 0 {
		elapsed = 5
	}
	totalPackets := vitaCountByID[routeID]
	totalBytes := vitaBytesByID[routeID]
	intervalPackets := totalPackets - vitaLastLogCountByID[routeID]
	intervalBytes := totalBytes - vitaLastLogBytesByID[routeID]
	pps := float64(intervalPackets) / elapsed
	maxGap := vitaMaxGapByID[routeID]
	vitaLastLogByID[routeID] = now
	vitaLastLogCountByID[routeID] = totalPackets
	vitaLastLogBytesByID[routeID] = totalBytes
	vitaMaxGapByID[routeID] = 0

	target := "127.0.0.1"
	if targetIP != nil {
		target = targetIP.String()
	}
	log.Printf("flexclient[%s]: received VITA proxy packet -> %s:%d (payload=%d bytes, total=%d, pps=%.1f, bytes=%d, max_gap=%s)",
		routeID, target, dstPort, payloadLen, totalPackets, pps, intervalBytes, maxGap.Truncate(time.Millisecond))
}

func shouldLogDiscoveryRewrite(routeID, serial string) bool {
	key := routeID + "|" + strings.ToLower(strings.TrimSpace(serial))
	now := time.Now()

	discoveryLogMu.Lock()
	defer discoveryLogMu.Unlock()
	if now.Sub(discoveryLastLog[key]) < 10*time.Second {
		return false
	}
	discoveryLastLog[key] = now
	return true
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

	if mode == radioModeOff {
		return
	}

	if mode == radioModeDirect {
		maybeSendDirectRadioPunch(routeID, serial, clientIP, payload, discoveryText)
		if directAssistEnabled() && serial != "" {
			if strings.TrimSpace(discoveryText) == "" {
				if _, text, err := extractDiscoveryText(payload); err == nil {
					discoveryText = text
				}
			}
			radioIP := strings.TrimSpace(fieldValue(discoveryText, "ip"))
			radioPort := broadcastPort
			if raw := strings.TrimSpace(fieldValue(discoveryText, "port")); raw != "" {
				if p, err := strconv.Atoi(raw); err == nil && p > 0 && p < 65536 {
					radioPort = p
				}
			}
			localIP, err := ensureLocalDirectAssistListener(serial, routeID, clientIP, radioIP, radioPort)
			if err != nil {
				log.Printf("flexclient[%s]: local direct assist setup failed serial=%s: %v", routeID, serial, err)
			} else if rewritten, _, err := rewriteDiscoveryForProxy(payload, localIP, broadcastPort); err == nil {
				if logRewrite && discoveryText != "" && shouldLogDiscoveryRewrite(routeID, serial) {
					log.Printf("flexclient[%s]: direct assist rewrite serial=%s %s:%d -> %s:%d",
						routeID, serial, radioIP, radioPort, localIP, broadcastPort)
				}
				rebroadcastDiscoveryPacket(rewritten)
				return
			}
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
		if now.Sub(entry.lastSeen) > discoveryCacheRebroadcastFreshAge {
			continue
		}
		score := routeRadioAffinityScore(route.IP, entry.radioIP)
		if !claimSerialOwnerWithScore(route.ID, entry.serial, now, score) {
			continue
		}
		markDiscovery(route.ID, entry.serial)
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
	_ = conn.SetReadBuffer(udpSocketBufferSize)
	_ = conn.SetWriteBuffer(udpSocketBufferSize)
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
		radioIP := strings.TrimSpace(fieldValue(discoveryText, "ip"))
		if !isModernDiscoveryPayload(discoveryText) {
			continue
		}

		now = time.Now()
		discoveryCache[serial] = cachedDiscovery{
			serial:   serial,
			radioIP:  radioIP,
			raw:      append([]byte(nil), payload...),
			lastSeen: now,
		}

		if IsRouteIgnored(route.ID) {
			continue
		}
		score := routeRadioAffinityScore(route.IP, radioIP)
		if !claimSerialOwnerWithScore(route.ID, serial, now, score) {
			continue
		}
		markDiscovery(route.ID, serial)
		applyDiscoveryModeAndBroadcast(conn, route.ID, clientIP, route.IP.String(), serial, payload, true, discoveryText)
	}
}
