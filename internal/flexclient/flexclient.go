package flexclient

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	serverPort        = 14992     // UDP port on each flextool server
	netbirdDefaultCLI = "netbird" // overridden by NETBIRD_CLI env
	helloInterval     = 30 * time.Second
	broadcastPort     = 4992 // FlexRadio discovery port on LAN
)

type Route struct {
	ID string
	IP net.IP
}

type routeStatus struct {
	lastHeartbeat time.Time
	lastDiscovery time.Time
}

var (
	routesMu sync.RWMutex
	routes   []Route

	statusMu       sync.RWMutex
	routeStatusMap = make(map[string]*routeStatus)
)

// CheckNetbirdStatus runs `netbird status` and returns whether we are connected
// to management, whether it explicitly needs login, the raw output, and any error.
func CheckNetbirdStatus() (connected bool, needsLogin bool, raw string, err error) {
	cmdPath := netbirdCLIPath()
	cmd := exec.Command(cmdPath, "status")
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}

	out, err := cmd.CombinedOutput()
	raw = string(out)

	if err != nil {
		log.Printf("flexclient: netbird status error: %v, output:\n%s", err, raw)
		return false, false, raw, err
	}

	// Example 1: "Daemon status: NeedsLogin"
	if strings.Contains(raw, "Daemon status: NeedsLogin") {
		log.Printf("flexclient: netbird status -> NeedsLogin")
		return false, true, raw, nil
	}

	// Example 2/3: look for Management line
	if strings.Contains(raw, "Management: Connected") {
		log.Printf("flexclient: netbird status -> Management: Connected")
		return true, false, raw, nil
	}

	if strings.Contains(raw, "Management: Disconnected") {
		log.Printf("flexclient: netbird status -> Management: Disconnected")
	}

	// Default: treat anything else as "not connected"
	log.Printf("flexclient: netbird status -> not connected, output:\n%s", raw)
	return false, false, raw, nil
}

// Start runs the flexclient engine until ctx is cancelled.
// version is included in HELLO messages.
func Start(ctx context.Context, version string) {
	log.Printf("flexclient: start (version=%s)", version)

	routesFound, err := discoverFlextoolRoutes()
	if err != nil {
		log.Printf("flexclient: discoverFlextoolRoutes error: %v", err)
		return
	}
	if len(routesFound) == 0 {
		log.Printf("flexclient: no Flextool routes found, nothing to do")
		return
	}

	initRouteStatusFor(routesFound)

	routesMu.Lock()
	routes = routesFound
	routesMu.Unlock()

	for _, r := range routesFound {
		route := r
		log.Printf("flexclient: launching server handler for route %s (%s)", route.ID, route.IP.String())
		go runForServer(ctx, route, version)
	}

	<-ctx.Done()
	log.Printf("flexclient: context cancelled, clearing state")

	routesMu.Lock()
	routes = nil
	routesMu.Unlock()

	statusMu.Lock()
	routeStatusMap = make(map[string]*routeStatus)
	statusMu.Unlock()
}

// Routes returns a snapshot of the currently known Flextool routes.
func Routes() []Route {
	routesMu.RLock()
	defer routesMu.RUnlock()

	out := make([]Route, len(routes))
	copy(out, routes)
	return out
}

// GetRouteStatus returns how long ago we saw a heartbeat and discovery packet
// for this route ID. hasHB / hasRX indicate if we have ever seen them.
func GetRouteStatus(routeID string) (heartbeatAgo, discoveryAgo time.Duration, hasHB, hasRX bool) {
	statusMu.RLock()
	defer statusMu.RUnlock()

	s, ok := routeStatusMap[routeID]
	if !ok {
		return 0, 0, false, false
	}
	if !s.lastHeartbeat.IsZero() {
		heartbeatAgo = time.Since(s.lastHeartbeat)
		hasHB = true
	}
	if !s.lastDiscovery.IsZero() {
		discoveryAgo = time.Since(s.lastDiscovery)
		hasRX = true
	}
	return
}

// --- internal helpers ---

func netbirdCLIPath() string {
	if p := os.Getenv("NETBIRD_CLI"); p != "" {
		return p
	}
	return netbirdDefaultCLI
}

func discoverFlextoolRoutes() ([]Route, error) {
	cmdPath := netbirdCLIPath()
	log.Printf("flexclient: running NetBird CLI: %s routes list", cmdPath)

	cmd := exec.Command(cmdPath, "routes", "list")
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
	out, err := cmd.Output()
	if err != nil {
		log.Printf("flexclient: error running 'netbird routes list': %v", err)
		return nil, fmt.Errorf("failed to run 'netbird routes list': %w", err)
	}

	var routes []Route
	scanner := bufio.NewScanner(bytes.NewReader(out))

	var currentID string
	var isFlextool bool

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// "ID: KC4CAW Flextool"
		if strings.Contains(line, "ID:") {
			idx := strings.Index(line, "ID:")
			if idx >= 0 {
				currentID = strings.TrimSpace(line[idx+len("ID:"):])
				isFlextool = strings.Contains(strings.ToLower(currentID), "flextool")
			}
			continue
		}

		// "Network: 10.10.2.5/32"
		if isFlextool && strings.Contains(line, "Network:") {
			idx := strings.Index(line, "Network:")
			if idx < 0 {
				continue
			}
			netStr := strings.TrimSpace(line[idx+len("Network:"):])
			parts := strings.SplitN(netStr, "/", 2)
			if len(parts) == 0 {
				continue
			}
			ipStr := strings.TrimSpace(parts[0])
			ip := net.ParseIP(ipStr)
			if ip == nil {
				log.Printf("flexclient: failed to parse IP from NetBird route: %q", ipStr)
				continue
			}

			routes = append(routes, Route{
				ID: currentID,
				IP: ip,
			})

			log.Printf("flexclient: discovered Flextool route: ID=%q IP=%s", currentID, ip.String())

			isFlextool = false
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("flexclient: scanner error parsing 'netbird routes list': %v", err)
		return nil, fmt.Errorf("scanner error parsing 'netbird routes list': %w", err)
	}

	log.Printf("flexclient: total Flextool routes discovered: %d", len(routes))
	return routes, nil
}

func initRouteStatusFor(rs []Route) {
	statusMu.Lock()
	defer statusMu.Unlock()

	routeStatusMap = make(map[string]*routeStatus)
	for _, r := range rs {
		routeStatusMap[r.ID] = &routeStatus{}
	}
	log.Printf("flexclient: initialized route status map for %d routes", len(rs))
}

func markHeartbeat(routeID string) {
	statusMu.Lock()
	defer statusMu.Unlock()

	s, ok := routeStatusMap[routeID]
	if !ok {
		s = &routeStatus{}
		routeStatusMap[routeID] = s
	}
	s.lastHeartbeat = time.Now()
}

func markDiscovery(routeID string) {
	statusMu.Lock()
	defer statusMu.Unlock()

	s, ok := routeStatusMap[routeID]
	if !ok {
		s = &routeStatus{}
		routeStatusMap[routeID] = s
	}
	s.lastDiscovery = time.Now()
}

func rebroadcastDiscoveryPacket(payload []byte) {
	dest := &net.UDPAddr{
		IP:   net.IPv4bcast, // 255.255.255.255
		Port: broadcastPort,
	}

	conn, err := net.DialUDP("udp", nil, dest)
	if err != nil {
		log.Printf("flexclient: rebroadcast DialUDP error: %v", err)
		return
	}
	defer conn.Close()

	if _, err := conn.Write(payload); err != nil {
		log.Printf("flexclient: rebroadcast Write error: %v", err)
	}
}

func runForServer(ctx context.Context, route Route, version string) {
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

	helloPayload := []byte(fmt.Sprintf(
		"HELLO client_ip=%s client_version=%s",
		clientIP, version,
	))

	// Initial HELLO
	if _, err := conn.Write(helloPayload); err != nil {
		log.Printf("flexclient[%s]: initial HELLO send failed: %v", route.ID, err)
	}

	// Keepalive HELLOs
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

	// Receive loop
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			log.Printf("flexclient[%s]: receive loop stopping (context cancelled)", route.ID)
			return
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

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

		markDiscovery(route.ID)
		rebroadcastDiscoveryPacket(payload)
	}
}
