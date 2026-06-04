package utils

import (
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
)

type radioEndpoint struct {
	Serial string
	IP     string
	Port   int
}

type proxySession struct {
	ID       uint64
	ClientIP string
	Serial   string
	RadioIP  string
	UDPPort  int
	LastSeen time.Time
	mu       sync.RWMutex
	closeNow func()
}

var (
	discoveredRadios       sync.Map // serial(lower) -> radioEndpoint
	proxyListeners         sync.Map // serial(lower) -> bool
	activeProxySessions    sync.Map // session ID(uint64) -> *proxySession
	selectedSerialByClient sync.Map // clientIP -> serial(lower)
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

func handleProxyConnection(clientConn net.Conn, serial string, co ConfigOptions) {
	defer clientConn.Close()

	v, ok := discoveredRadios.Load(serial)
	if !ok {
		fmt.Printf("[PROXY] serial %s is unknown; dropping connection from %s\n", serial, clientConn.RemoteAddr().String())
		return
	}
	ep := v.(radioEndpoint)

	radioConn, err := net.DialTimeout("tcp", net.JoinHostPort(ep.IP, strconv.Itoa(ep.Port)), 7*time.Second)
	if err != nil {
		fmt.Printf("[PROXY] failed to connect to radio %s:%d for serial=%s: %v\n", ep.IP, ep.Port, serial, err)
		return
	}
	defer radioConn.Close()

	clientHost, _, err := net.SplitHostPort(clientConn.RemoteAddr().String())
	if err != nil {
		clientHost = clientConn.RemoteAddr().String()
	}

	// In single-proxy mode, preserve the old behavior of replacing any prior
	// session for this radio/client. In multi-proxy mode, SmartSDR, CAT, and DAX
	// may all keep concurrent TCP API sockets to the same radio.
	if !co.MultiProxy {
		closeSessionForRadio(ep.IP)
		closeConflictingSessionsForClient(clientHost, serial, ep.IP, false)
	}

	session := &proxySession{
		ID:       nextProxySessionID.Add(1),
		ClientIP: clientHost,
		Serial:   serial,
		RadioIP:  ep.IP,
		UDPPort:  4991, // default until client udpport command is seen
		LastSeen: time.Now(),
	}
	activeProxySessions.Store(session.ID, session)

	fmt.Printf("[PROXY] session start id=%d serial=%s client=%s radio=%s:%d\n", session.ID, serial, clientHost, ep.IP, ep.Port)

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
	radioIP = strings.TrimSpace(radioIP)
	if radioIP == "" {
		return nil
	}

	var exact []VitaProxyTarget
	var fallback *proxySession
	var fallbackLastSeen time.Time

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
		serial := normalizeSerial(s.Serial)
		udpPort := s.UDPPort
		lastSeen := s.LastSeen
		s.mu.RUnlock()

		if time.Since(lastSeen) > 30*time.Second {
			return true
		}
		if clientIP == "" || udpPort <= 0 {
			return true
		}
		if selectedSerial, hasSelection := getSelectedSerialForClient(clientIP); hasSelection {
			if normalizeSerial(selectedSerial) != serial {
				return true
			}
		}

		target := VitaProxyTarget{ClientIP: clientIP, Port: udpPort}
		if radioDstPort > 0 && udpPort == radioDstPort {
			exact = append(exact, target)
			return true
		}
		if fallback == nil || lastSeen.After(fallbackLastSeen) {
			fallback = s
			fallbackLastSeen = lastSeen
		}
		return true
	})

	if len(exact) > 0 {
		return exact
	}
	if fallback == nil {
		return nil
	}

	fallback.mu.RLock()
	defer fallback.mu.RUnlock()
	return []VitaProxyTarget{{ClientIP: strings.TrimSpace(fallback.ClientIP), Port: fallback.UDPPort}}
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
