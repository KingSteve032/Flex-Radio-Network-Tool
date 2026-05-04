package utils

import (
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
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
	ClientIP string
	Serial   string
	RadioIP  string
	UDPPort  int
	mu       sync.RWMutex
	closeNow func()
}

var (
	discoveredRadios       sync.Map // serial(lower) -> radioEndpoint
	proxyListeners         sync.Map // serial(lower) -> bool
	sessionsByRadio        sync.Map // radioIP -> *proxySession
	selectedSerialByClient sync.Map // clientIP -> serial(lower)
)

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

	// Ensure only one active session per radio endpoint. If a stale or duplicate
	// connection exists, replace it.
	closeSessionForRadio(ep.IP)

	// SmartSDR may keep stale TCP proxy sockets around briefly when switching
	// radios. In single-proxy mode we close all other sessions for this client.
	// In multi-proxy mode we only close conflicting duplicate sessions.
	closeConflictingSessionsForClient(clientHost, serial, ep.IP, co.MultiProxy)

	session := &proxySession{
		ClientIP: clientHost,
		Serial:   serial,
		RadioIP:  ep.IP,
		UDPPort:  4991, // default until client udpport command is seen
	}
	sessionsByRadio.Store(ep.IP, session)

	fmt.Printf("[PROXY] session start serial=%s client=%s radio=%s:%d\n", serial, clientHost, ep.IP, ep.Port)

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
		_, _ = io.Copy(clientConn, radioConn)
		closeBoth()
	}()

	wg.Wait()
	deleteSessionIfCurrent(ep.IP, session)

	fmt.Printf("[PROXY] session end serial=%s client=%s\n", serial, clientHost)
}

func closeConflictingSessionsForClient(clientIP, serial, radioIP string, allowMulti bool) {
	clientIP = strings.TrimSpace(clientIP)
	serial = normalizeSerial(serial)
	radioIP = strings.TrimSpace(radioIP)
	if clientIP == "" {
		return
	}

	sessionsByRadio.Range(func(_, value any) bool {
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
		if !allowMulti {
			shouldClose = true
		}
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
	v, ok := sessionsByRadio.Load(radioIP)
	if !ok {
		return
	}
	s, ok := v.(*proxySession)
	if !ok || s == nil {
		return
	}
	if s.closeNow != nil {
		fmt.Printf("[PROXY] replacing existing session radio=%s client=%s serial=%s\n", radioIP, s.ClientIP, s.Serial)
		s.closeNow()
	}
}

func deleteSessionIfCurrent(radioIP string, current *proxySession) {
	radioIP = strings.TrimSpace(radioIP)
	if radioIP == "" || current == nil {
		return
	}
	v, ok := sessionsByRadio.Load(radioIP)
	if !ok {
		return
	}
	s, ok := v.(*proxySession)
	if !ok {
		return
	}
	if s == current {
		sessionsByRadio.Delete(radioIP)
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

	chunk := strings.ToLower(string(p))
	s.buf += chunk
	if len(s.buf) > 1024 {
		s.buf = s.buf[len(s.buf)-1024:]
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
			s.session.mu.Unlock()
			fmt.Printf("[PROXY] serial=%s client udpport=%d\n", s.session.Serial, port)
		}

		s.buf = s.buf[end:]
	}

	return len(p), nil
}

func proxyClientToRadioWithUDPPortTracking(src net.Conn, dst net.Conn, session *proxySession, _ bool) {
	sniffer := &udpPortSniffer{session: session}
	_, _ = io.Copy(io.MultiWriter(dst, sniffer), src)
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

func GetVitaProxyTarget(radioIP string) (clientIP string, clientPort int, ok bool) {
	v, found := sessionsByRadio.Load(radioIP)
	if !found {
		return "", 0, false
	}
	s := v.(*proxySession)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ClientIP == "" || s.UDPPort <= 0 {
		return "", 0, false
	}
	return s.ClientIP, s.UDPPort, true
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
