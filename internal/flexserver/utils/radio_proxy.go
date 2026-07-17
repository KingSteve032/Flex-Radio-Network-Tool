package utils

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultProxyBasePort = 30000
	proxyPortSpan        = 20000
	vitaTxPacketMagicV1  = "VITATX1"
	vitaTxPacketMagicV2  = "VITATX2"
)

type radioEndpoint struct {
	Serial string
	IP     string
	Port   int
}

type proxySession struct {
	ID          uint64
	ClientIP    string
	Serial      string
	RadioIP     string
	SourceLANIP string
	UDPPort     int
	LastSeen    time.Time
	mu          sync.RWMutex
	closeNow    func()
}

var (
	discoveredRadios       sync.Map // serial(lower) -> radioEndpoint
	proxyListeners         sync.Map // serial(lower) -> bool
	activeProxySessions    sync.Map // session ID(uint64) -> *proxySession
	selectedSerialByClient sync.Map // clientIP -> serial(lower)
	proxyLANAssignMu       sync.Mutex
	pendingProxyLANSources map[string]int
	vitaTXConns            sync.Map // sourceIP->radioIP:port -> *net.UDPConn
	nextProxySessionID     atomic.Uint64
)

type VitaProxyTarget struct {
	ClientIP string
	Port     int
}

func normalizeSerial(serial string) string {
	return strings.ToLower(strings.TrimSpace(serial))
}

func proxyPortForSerial(serial string, basePort int) int {
	if basePort <= 0 {
		basePort = defaultProxyBasePort
	}
	sum := crc32.ChecksumIEEE([]byte(normalizeSerial(serial)))
	return basePort + int(sum%proxyPortSpan)
}

func RegisterDiscoveredRadio(serial, ip string, port int, co ConfigOptions) {
	serial = normalizeSerial(serial)
	if serial == "" || ip == "" || port <= 0 {
		return
	}

	discoveredRadios.Store(serial, radioEndpoint{
		Serial: serial,
		IP:     ip,
		Port:   port,
	})

	startTCPProxyListener(serial, co)
}

func SetClientSelectedProxySerial(clientIP, serial string) {
	clientIP = strings.TrimSpace(clientIP)
	serial = normalizeSerial(serial)
	if clientIP == "" || serial == "" {
		return
	}
	selectedSerialByClient.Store(clientIP, serial)
}

func getSelectedSerialForClient(clientIP string) (string, bool) {
	clientIP = strings.TrimSpace(clientIP)
	if clientIP == "" {
		return "", false
	}
	v, ok := selectedSerialByClient.Load(clientIP)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func clearSelectedSerialForClient(clientIP string) {
	clientIP = strings.TrimSpace(clientIP)
	if clientIP == "" {
		return
	}
	selectedSerialByClient.Delete(clientIP)
}

func getAnyDiscoveredSerial() (string, bool) {
	found := ""
	discoveredRadios.Range(func(key, _ any) bool {
		if s, ok := key.(string); ok && s != "" {
			found = s
			return false
		}
		return true
	})
	if found == "" {
		return "", false
	}
	return found, true
}

// StartSharedProxyListener starts a single SmartSDR-compatible TCP listener on
// port 4992. SmartSDR appears to target 4992 regardless of discovery "port"
// field, so we route by the client's selected serial.
func StartSharedProxyListener(co ConfigOptions) {
	const sharedPort = 4992
	const key = "__shared_4992__"
	if _, loaded := proxyListeners.LoadOrStore(key, true); loaded {
		return
	}

	listenIP := "0.0.0.0"
	if co.SendNetworkInterface.IPAddress != nil && co.SendNetworkInterface.IPAddress.String() != "" {
		listenIP = co.SendNetworkInterface.IPAddress.String()
	}
	addr := net.JoinHostPort(listenIP, strconv.Itoa(sharedPort))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		proxyListeners.Delete(key)
		fmt.Printf("[PROXY] failed to listen shared proxy on %s: %v\n", addr, err)
		return
	}
	fmt.Printf("[PROXY] shared listener active on %s\n", addr)

	go func() {
		for {
			clientConn, err := ln.Accept()
			if err != nil {
				continue
			}

			clientHost, _, err := net.SplitHostPort(clientConn.RemoteAddr().String())
			if err != nil {
				clientHost = clientConn.RemoteAddr().String()
			}

			serial, ok := getSelectedSerialForClient(clientHost)
			if !ok {
				serial, ok = getAnyDiscoveredSerial()
			}
			if !ok || serial == "" {
				fmt.Printf("[PROXY] no selected/discovered serial for client=%s; dropping shared proxy connect\n", clientHost)
				_ = clientConn.Close()
				continue
			}

			go handleProxyConnection(clientConn, serial, co)
		}
	}()
}

func startTCPProxyListener(serial string, co ConfigOptions) {
	if _, loaded := proxyListeners.LoadOrStore(serial, true); loaded {
		return
	}

	proxyPort := proxyPortForSerial(serial, co.ProxyBasePort)
	listenIP := "0.0.0.0"
	if co.SendNetworkInterface.IPAddress != nil && co.SendNetworkInterface.IPAddress.String() != "" {
		listenIP = co.SendNetworkInterface.IPAddress.String()
	}
	addr := net.JoinHostPort(listenIP, strconv.Itoa(proxyPort))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		proxyListeners.Delete(serial)
		fmt.Printf("[PROXY] failed to listen for serial=%s on %s: %v\n", serial, addr, err)
		return
	}

	if co.EnableDebug {
		fmt.Printf("[PROXY] listening for serial=%s on %s\n", serial, addr)
	}

	go func() {
		for {
			clientConn, err := ln.Accept()
			if err != nil {
				continue
			}
			go handleProxyConnection(clientConn, serial, co)
		}
	}()
}

func normalizeIPString(ip string) string {
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return strings.TrimSpace(ip)
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	return parsed.String()
}

func proxyLANSourcePool(co ConfigOptions) []string {
	out := make([]string, 0, len(co.ProxyLANSourceIPs))
	seen := map[string]bool{}
	for _, ip := range co.ProxyLANSourceIPs {
		if ip == nil {
			continue
		}
		s := normalizeIPString(ip.String())
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func defaultProxyLANSourceIP(co ConfigOptions) string {
	if co.SendNetworkInterface.IPAddress == nil {
		return ""
	}
	return normalizeIPString(co.SendNetworkInterface.IPAddress.String())
}

func chooseProxyLANSourceIPForSession(clientIP, serial string, co ConfigOptions) string {
	clientIP = strings.TrimSpace(clientIP)
	serial = normalizeSerial(serial)
	pool := proxyLANSourcePool(co)
	if len(pool) == 0 {
		return defaultProxyLANSourceIP(co)
	}
	if clientIP == "" {
		return ""
	}

	proxyLANAssignMu.Lock()
	defer proxyLANAssignMu.Unlock()

	leaseCounts := map[string]int{}
	for _, ip := range pool {
		leaseCounts[ip] = 0
	}
	activeProxySessions.Range(func(_, value any) bool {
		s, ok := value.(*proxySession)
		if !ok || s == nil {
			return true
		}

		s.mu.RLock()
		sourceLANIP := normalizeIPString(s.SourceLANIP)
		s.mu.RUnlock()
		if _, ok := leaseCounts[sourceLANIP]; ok {
			leaseCounts[sourceLANIP]++
		}
		return true
	})
	for sourceLANIP, count := range pendingProxyLANSources {
		if _, ok := leaseCounts[sourceLANIP]; ok {
			leaseCounts[sourceLANIP] += count
		}
	}

	chosen := pool[0]
	for _, candidate := range pool[1:] {
		if leaseCounts[candidate] < leaseCounts[chosen] {
			chosen = candidate
		}
	}
	if pendingProxyLANSources == nil {
		pendingProxyLANSources = map[string]int{}
	}
	pendingProxyLANSources[chosen]++
	return chosen
}

func releasePendingProxyLANSource(sourceLANIP string) {
	sourceLANIP = normalizeIPString(sourceLANIP)
	if sourceLANIP == "" {
		return
	}
	proxyLANAssignMu.Lock()
	defer proxyLANAssignMu.Unlock()
	if pendingProxyLANSources == nil {
		return
	}
	if pendingProxyLANSources[sourceLANIP] <= 1 {
		delete(pendingProxyLANSources, sourceLANIP)
		return
	}
	pendingProxyLANSources[sourceLANIP]--
}

func dialRadioTCP(ep radioEndpoint, sourceLANIP string) (net.Conn, error) {
	dest := net.JoinHostPort(ep.IP, strconv.Itoa(ep.Port))
	if strings.TrimSpace(sourceLANIP) == "" {
		return net.DialTimeout("tcp", dest, 7*time.Second)
	}

	localIP := net.ParseIP(sourceLANIP)
	if localIP == nil {
		return nil, fmt.Errorf("invalid proxy LAN source IP %q", sourceLANIP)
	}
	dialer := net.Dialer{
		Timeout:   7 * time.Second,
		LocalAddr: &net.TCPAddr{IP: localIP, Port: 0},
	}
	return dialer.Dial("tcp", dest)
}

func connLocalIP(conn net.Conn) string {
	if conn == nil || conn.LocalAddr() == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return ""
	}
	return normalizeIPString(host)
}

func handleProxyConnection(clientConn net.Conn, serial string, co ConfigOptions) {
	defer clientConn.Close()

	v, ok := discoveredRadios.Load(serial)
	if !ok {
		fmt.Printf("[PROXY] serial %s is unknown; dropping connection from %s\n", serial, clientConn.RemoteAddr().String())
		return
	}
	ep := v.(radioEndpoint)

	clientHost, _, err := net.SplitHostPort(clientConn.RemoteAddr().String())
	if err != nil {
		clientHost = clientConn.RemoteAddr().String()
	}

	sourceLANIP := chooseProxyLANSourceIPForSession(clientHost, serial, co)
	if sourceLANIP != "" {
		defer releasePendingProxyLANSource(sourceLANIP)
	}

	// In single-proxy mode, preserve the old behavior of replacing any prior
	// session for this radio/client. In multi-proxy mode, SmartSDR, CAT, and DAX
	// may all keep concurrent TCP API sockets to the same radio.
	if !co.MultiProxy {
		closeSessionForRadio(ep.IP)
		closeConflictingSessionsForClient(clientHost, serial, ep.IP, false)
	}

	radioConn, err := dialRadioTCP(ep, sourceLANIP)
	if err != nil {
		if sourceLANIP != "" {
			fmt.Printf("[PROXY] failed to connect to radio %s:%d for serial=%s source_lan_ip=%s: %v\n", ep.IP, ep.Port, serial, sourceLANIP, err)
		} else {
			fmt.Printf("[PROXY] failed to connect to radio %s:%d for serial=%s: %v\n", ep.IP, ep.Port, serial, err)
		}
		return
	}
	defer radioConn.Close()
	if sourceLANIP == "" {
		sourceLANIP = connLocalIP(radioConn)
	}

	session := &proxySession{
		ID:          nextProxySessionID.Add(1),
		ClientIP:    clientHost,
		Serial:      serial,
		RadioIP:     ep.IP,
		SourceLANIP: sourceLANIP,
		UDPPort:     4991, // default until client udpport command is seen
		LastSeen:    time.Now(),
	}
	activeProxySessions.Store(session.ID, session)

	fmt.Printf("[PROXY] session start id=%d serial=%s client=%s radio=%s:%d source_lan_ip=%s\n", session.ID, serial, clientHost, ep.IP, ep.Port, sourceLANIP)

	var wg sync.WaitGroup
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = clientConn.Close()
			_ = radioConn.Close()
		})
	}
	session.closeNow = closeBoth
	wg.Add(2)

	go func() {
		defer wg.Done()
		proxyClientToRadioWithUDPPortTracking(clientConn, radioConn, session, co.EnableDebug)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(io.MultiWriter(clientConn, &sessionActivityWriter{session: session}), radioConn)
		closeBoth()
	}()

	wg.Wait()
	deleteSessionIfCurrent(session)

	fmt.Printf("[PROXY] session end id=%d serial=%s client=%s\n", session.ID, serial, clientHost)
}

func closeConflictingSessionsForClient(clientIP, serial, radioIP string, allowMulti bool) {
	clientIP = strings.TrimSpace(clientIP)
	serial = normalizeSerial(serial)
	radioIP = strings.TrimSpace(radioIP)
	if clientIP == "" {
		return
	}
	if allowMulti {
		return
	}

	activeProxySessions.Range(func(_, value any) bool {
		s, ok := value.(*proxySession)
		if !ok || s == nil {
			return true
		}
		if strings.TrimSpace(s.ClientIP) != clientIP {
			return true
		}

		sameRadio := radioIP != "" && strings.TrimSpace(s.RadioIP) == radioIP
		sameSerial := serial != "" && normalizeSerial(s.Serial) == serial
		shouldClose := sameRadio || sameSerial
		shouldClose = shouldClose || !allowMulti
		if !shouldClose {
			return true
		}

		if s.closeNow != nil {
			fmt.Printf("[PROXY] closing stale session client=%s serial=%s radio=%s\n", clientIP, s.Serial, s.RadioIP)
			s.closeNow()
		}
		return true
	})
}

func closeSessionForRadio(radioIP string) {
	radioIP = strings.TrimSpace(radioIP)
	if radioIP == "" {
		return
	}
	activeProxySessions.Range(func(_, value any) bool {
		s, ok := value.(*proxySession)
		if !ok || s == nil {
			return true
		}
		if strings.TrimSpace(s.RadioIP) != radioIP {
			return true
		}
		if s.closeNow != nil {
			fmt.Printf("[PROXY] replacing existing session radio=%s client=%s serial=%s\n", radioIP, s.ClientIP, s.Serial)
			s.closeNow()
		}
		return true
	})
}

func deleteSessionIfCurrent(current *proxySession) {
	if current == nil {
		return
	}
	activeProxySessions.Delete(current.ID)
}

func terminateSession(session *proxySession, reason string) {
	if session == nil {
		return
	}
	if reason != "" {
		fmt.Printf("[PROXY] terminating session serial=%s client=%s radio=%s reason=%s\n",
			session.Serial, session.ClientIP, session.RadioIP, reason)
	}
	clearSelectedSerialForClient(session.ClientIP)
	deleteSessionIfCurrent(session)
	if session.closeNow != nil {
		session.closeNow()
	}
}

type udpPortSniffer struct {
	session *proxySession
	buf     string
}

func (s *udpPortSniffer) Write(p []byte) (int, error) {
	if s == nil || s.session == nil || len(p) == 0 {
		return len(p), nil
	}
	s.session.mu.Lock()
	s.session.LastSeen = time.Now()
	s.session.mu.Unlock()

	chunk := strings.ToLower(string(p))
	s.buf += chunk
	if len(s.buf) > 4096 {
		s.buf = s.buf[len(s.buf)-4096:]
	}

	const needle = "client udpport "
	for {
		idx := strings.Index(s.buf, needle)
		if idx < 0 {
			break
		}

		start := idx + len(needle)
		end := start
		for end < len(s.buf) && s.buf[end] >= '0' && s.buf[end] <= '9' {
			end++
		}

		if end == start {
			s.buf = s.buf[start:]
			continue
		}

		pRaw := s.buf[start:end]
		if port, err := strconv.Atoi(pRaw); err == nil && port > 0 && port < 65536 {
			s.session.mu.Lock()
			s.session.UDPPort = port
			s.session.LastSeen = time.Now()
			s.session.mu.Unlock()
			fmt.Printf("[PROXY] serial=%s client udpport=%d\n", s.session.Serial, port)
		}

		s.buf = s.buf[end:]
	}

	// SmartSDR sends plain-text TCP commands. If we see a disconnect command,
	// terminate this proxy session immediately so stale VITA does not continue
	// after a rapid radio switch.
	if strings.Contains(s.buf, "disconnect") {
		terminateSession(s.session, "disconnect command observed")
	}

	return len(p), nil
}

func proxyClientToRadioWithUDPPortTracking(src net.Conn, dst net.Conn, session *proxySession, _ bool) {
	sniffer := &udpPortSniffer{session: session}
	_, _ = io.Copy(io.MultiWriter(dst, sniffer), src)
}

type sessionActivityWriter struct {
	session *proxySession
}

func (w *sessionActivityWriter) Write(p []byte) (int, error) {
	if w == nil || w.session == nil {
		return len(p), nil
	}
	w.session.mu.Lock()
	w.session.LastSeen = time.Now()
	w.session.mu.Unlock()
	return len(p), nil
}

func parseClientUDPPortCommand(line []byte) (int, bool) {
	s := strings.ToLower(string(line))
	needle := "client udpport "
	idx := strings.Index(s, needle)
	if idx < 0 {
		return 0, false
	}

	rest := s[idx+len(needle):]
	rest = strings.TrimSpace(rest)
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, false
	}

	p, err := strconv.Atoi(fields[0])
	if err != nil || p <= 0 || p >= 65536 {
		return 0, false
	}
	return p, true
}

func GetVitaProxyTargets(radioIP string, radioDstPort int) []VitaProxyTarget {
	return GetVitaProxyTargetsForDestination(radioIP, "", radioDstPort)
}

func GetVitaProxyTargetsForDestination(radioIP, serverDstIP string, radioDstPort int) []VitaProxyTarget {
	radioIP = strings.TrimSpace(radioIP)
	serverDstIP = normalizeIPString(serverDstIP)
	if radioIP == "" {
		return nil
	}

	var exact []VitaProxyTarget
	exactSeen := map[string]bool{}
	fallbackByClient := map[string]proxySessionSnapshot{}

	activeProxySessions.Range(func(_, value any) bool {
		s, ok := value.(*proxySession)
		if !ok || s == nil {
			return true
		}
		if strings.TrimSpace(s.RadioIP) != radioIP {
			return true
		}

		s.mu.RLock()
		clientIP := strings.TrimSpace(s.ClientIP)
		sourceLANIP := normalizeIPString(s.SourceLANIP)
		udpPort := s.UDPPort
		lastSeen := s.LastSeen
		s.mu.RUnlock()

		if serverDstIP != "" && sourceLANIP != "" && sourceLANIP != serverDstIP {
			return true
		}
		if time.Since(lastSeen) > 30*time.Second {
			return true
		}
		if clientIP == "" || udpPort <= 0 {
			return true
		}
		target := VitaProxyTarget{ClientIP: clientIP, Port: udpPort}
		if radioDstPort > 0 && udpPort == radioDstPort {
			key := target.ClientIP + ":" + strconv.Itoa(target.Port)
			if !exactSeen[key] {
				exactSeen[key] = true
				exact = append(exact, target)
			}
			return true
		}
		if prev, ok := fallbackByClient[clientIP]; !ok || lastSeen.After(prev.LastSeen) {
			fallbackByClient[clientIP] = proxySessionSnapshot{
				ClientIP: clientIP,
				UDPPort:  udpPort,
				LastSeen: lastSeen,
			}
		}
		return true
	})

	if len(exact) > 0 {
		return exact
	}
	if len(fallbackByClient) == 0 {
		return nil
	}

	out := make([]VitaProxyTarget, 0, len(fallbackByClient))
	for _, s := range fallbackByClient {
		out = append(out, VitaProxyTarget{ClientIP: s.ClientIP, Port: s.UDPPort})
	}
	return out
}

type proxySessionSnapshot struct {
	ClientIP string
	UDPPort  int
	LastSeen time.Time
}

func findProxyLANSourceIPForClientSerialUDPPort(clientIP, serial string, udpPort int) string {
	clientIP = strings.TrimSpace(clientIP)
	serial = normalizeSerial(serial)
	if clientIP == "" || serial == "" {
		return ""
	}

	var sourceLANIP string
	var lastSeen time.Time
	activeProxySessions.Range(func(_, value any) bool {
		s, ok := value.(*proxySession)
		if !ok || s == nil {
			return true
		}

		s.mu.RLock()
		sClientIP := strings.TrimSpace(s.ClientIP)
		sSerial := normalizeSerial(s.Serial)
		sSourceLANIP := normalizeIPString(s.SourceLANIP)
		sUDPPort := s.UDPPort
		sLastSeen := s.LastSeen
		s.mu.RUnlock()

		if sClientIP != clientIP || sSerial != serial || sSourceLANIP == "" {
			return true
		}
		if udpPort > 0 && sUDPPort != udpPort {
			return true
		}
		if sourceLANIP == "" || sLastSeen.After(lastSeen) {
			sourceLANIP = sSourceLANIP
			lastSeen = sLastSeen
		}
		return true
	})
	return sourceLANIP
}

func parseVitaTXPayload(payload []byte) (serial string, srcUDPPort int, data []byte, ok bool) {
	if len(payload) < len(vitaTxPacketMagicV1)+1 {
		return "", 0, nil, false
	}

	if bytes.HasPrefix(payload, []byte(vitaTxPacketMagicV2)) {
		offset := len(vitaTxPacketMagicV2)
		serialLen := int(payload[offset])
		offset++
		if serialLen <= 0 || len(payload) < offset+serialLen+3 {
			return "", 0, nil, false
		}
		serial = normalizeSerial(string(payload[offset : offset+serialLen]))
		offset += serialLen
		srcUDPPort = int(binary.BigEndian.Uint16(payload[offset : offset+2]))
		data = payload[offset+2:]
		if serial == "" || srcUDPPort <= 0 || len(data) == 0 {
			return "", 0, nil, false
		}
		return serial, srcUDPPort, data, true
	}

	if !bytes.HasPrefix(payload, []byte(vitaTxPacketMagicV1)) {
		return "", 0, nil, false
	}
	offset := len(vitaTxPacketMagicV1)
	serialLen := int(payload[offset])
	offset++
	if serialLen <= 0 || len(payload) < offset+serialLen+1 {
		return "", 0, nil, false
	}
	serial = normalizeSerial(string(payload[offset : offset+serialLen]))
	data = payload[offset+serialLen:]
	if serial == "" || len(data) == 0 {
		return "", 0, nil, false
	}
	return serial, 0, data, true
}

func noteProxySessionActivity(clientIP, serial string) {
	clientIP = strings.TrimSpace(clientIP)
	serial = normalizeSerial(serial)
	if clientIP == "" || serial == "" {
		return
	}
	activeProxySessions.Range(func(_, value any) bool {
		s, ok := value.(*proxySession)
		if !ok || s == nil {
			return true
		}
		if strings.TrimSpace(s.ClientIP) != clientIP || normalizeSerial(s.Serial) != serial {
			return true
		}
		s.mu.Lock()
		s.LastSeen = time.Now()
		s.mu.Unlock()
		return true
	})
}

func HandleClientVitaTX(clientIP string, co ConfigOptions, payload []byte) bool {
	serial, srcUDPPort, data, ok := parseVitaTXPayload(payload)
	if !ok {
		return false
	}
	if !co.EnableVitaProxy {
		return true
	}
	clientIP = strings.TrimSpace(clientIP)
	if clientIP == "" {
		return true
	}
	if _, ok := getClientInfo(clientIP); !ok {
		return true
	}

	v, ok := discoveredRadios.Load(serial)
	if !ok {
		if co.EnableDebug {
			fmt.Printf("[VITA-TX] unknown serial=%s from client=%s\n", serial, clientIP)
		}
		return true
	}
	ep, ok := v.(radioEndpoint)
	if !ok || strings.TrimSpace(ep.IP) == "" {
		return true
	}

	port := co.VitaProxyPort
	if port <= 0 || port >= 65536 {
		port = 4991
	}
	sourceLANIP := findProxyLANSourceIPForClientSerialUDPPort(clientIP, serial, srcUDPPort)
	if sourceLANIP == "" {
		sourceLANIP = findProxyLANSourceIPForClientSerialUDPPort(clientIP, serial, 0)
	}
	if sourceLANIP == "" {
		sourceLANIP = chooseProxyLANSourceIPForSession(clientIP, serial, co)
		if sourceLANIP != "" {
			defer releasePendingProxyLANSource(sourceLANIP)
		}
	}
	dest := &net.UDPAddr{IP: net.ParseIP(ep.IP), Port: port}
	if dest.IP == nil {
		return true
	}

	conn, err := getVitaTXConn(sourceLANIP, dest)
	if err != nil {
		if co.EnableDebug {
			fmt.Printf("[VITA-TX] dial radio failed client=%s serial=%s source_lan_ip=%s dest=%s err=%v\n", clientIP, serial, sourceLANIP, dest.String(), err)
		}
		return true
	}

	if _, err := conn.Write(data); err != nil {
		vitaTXConns.Delete(vitaTXConnKey(sourceLANIP, dest))
		_ = conn.Close()
		if co.EnableDebug {
			fmt.Printf("[VITA-TX] send radio failed client=%s serial=%s source_lan_ip=%s dest=%s err=%v\n", clientIP, serial, sourceLANIP, dest.String(), err)
		}
		return true
	}
	noteProxySessionActivity(clientIP, serial)
	if co.EnableDebug {
		fmt.Printf("[VITA-TX] client=%s serial=%s source_lan_ip=%s -> %s bytes=%d\n", clientIP, serial, sourceLANIP, dest.String(), len(data))
	}
	return true
}

func vitaTXConnKey(sourceLANIP string, dest *net.UDPAddr) string {
	sourceLANIP = normalizeIPString(sourceLANIP)
	if dest == nil {
		return sourceLANIP + "->"
	}
	return sourceLANIP + "->" + dest.String()
}

func getVitaTXConn(sourceLANIP string, dest *net.UDPAddr) (*net.UDPConn, error) {
	if dest == nil || dest.IP == nil || dest.Port <= 0 {
		return nil, fmt.Errorf("invalid VITA TX destination")
	}
	sourceLANIP = normalizeIPString(sourceLANIP)
	key := vitaTXConnKey(sourceLANIP, dest)
	if v, ok := vitaTXConns.Load(key); ok {
		if conn, ok := v.(*net.UDPConn); ok && conn != nil {
			return conn, nil
		}
	}

	var localAddr *net.UDPAddr
	if sourceLANIP != "" {
		localIP := net.ParseIP(sourceLANIP)
		if localIP == nil {
			return nil, fmt.Errorf("invalid proxy LAN source IP %q", sourceLANIP)
		}
		localAddr = &net.UDPAddr{IP: localIP, Port: 0}
	}
	conn, err := net.DialUDP("udp", localAddr, dest)
	if err != nil {
		return nil, err
	}
	_ = conn.SetWriteBuffer(udpSocketBufferSize)
	actual, loaded := vitaTXConns.LoadOrStore(key, conn)
	if loaded {
		_ = conn.Close()
		if existing, ok := actual.(*net.UDPConn); ok && existing != nil {
			return existing, nil
		}
		return nil, fmt.Errorf("invalid cached VITA TX connection")
	}
	return conn, nil
}

func GetVitaProxyTarget(radioIP string) (clientIP string, clientPort int, ok bool) {
	targets := GetVitaProxyTargets(radioIP, 0)
	if len(targets) == 0 {
		return "", 0, false
	}
	return targets[0].ClientIP, targets[0].Port, true
}

// IsKnownRadioIP reports whether this IP matches any discovered radio endpoint.
func IsKnownRadioIP(ip string) bool {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return false
	}

	found := false
	discoveredRadios.Range(func(_ any, value any) bool {
		ep, ok := value.(radioEndpoint)
		if !ok {
			return true
		}
		if strings.TrimSpace(ep.IP) == ip {
			found = true
			return false
		}
		return true
	})
	return found
}
