package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

const (
	Version             = "0.1.0"   // flexclient version
	serverPort          = 14992     // UDP port on each flextool server
	netbirdDefaultCLI   = "netbird" // overridden by NETBIRD_CLI env
	helloInterval       = 30        // seconds between HELLOs
	broadcastPort       = 4992      // FlexRadio discovery port on LAN
	heartbeatListUpdate = 1 * time.Second
	discoveryActiveFor  = 10 * time.Second // RX "active" window
)

type flextoolRoute struct {
	ID string
	IP net.IP
}

type routeStatus struct {
	lastHeartbeat time.Time
	lastDiscovery time.Time
}

var (
	routesMu sync.RWMutex
	routes   []flextoolRoute

	statusMu       sync.RWMutex
	routeStatusMap = make(map[string]*routeStatus)

	clientMu      sync.Mutex
	clientCancel  context.CancelFunc
	clientRunning bool
)

// --- NetBird route discovery ---

func netbirdCLIPath() string {
	if p := os.Getenv("NETBIRD_CLI"); p != "" {
		return p
	}
	return netbirdDefaultCLI
}

func discoverFlextoolRoutes() ([]flextoolRoute, error) {
	cmd := exec.Command(netbirdCLIPath(), "routes", "list")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run 'netbird routes list': %w", err)
	}

	var routes []flextoolRoute
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
				continue
			}

			routes = append(routes, flextoolRoute{
				ID: currentID,
				IP: ip,
			})

			// done with this ID
			isFlextool = false
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error parsing 'netbird routes list': %w", err)
	}

	return routes, nil
}

// --- Status helpers ---

func initRouteStatusFor(routes []flextoolRoute) {
	statusMu.Lock()
	defer statusMu.Unlock()
	routeStatusMap = make(map[string]*routeStatus)
	for _, r := range routes {
		routeStatusMap[r.ID] = &routeStatus{}
	}
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

func getRouteStatus(routeID string) (heartbeatAgo, discoveryAgo time.Duration, hasHB, hasRX bool) {
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

// --- Rebroadcast ---

func rebroadcastDiscoveryPacket(payload []byte) {
	dest := &net.UDPAddr{
		IP:   net.IPv4bcast, // 255.255.255.255
		Port: broadcastPort,
	}

	conn, err := net.DialUDP("udp", nil, dest)
	if err != nil {
		return
	}
	defer conn.Close()

	_, _ = conn.Write(payload) // ignore error here; nothing to show in GUI anyway
}

// One goroutine per Flextool server
func runForServer(ctx context.Context, route flextoolRoute) {
	serverAddr := &net.UDPAddr{
		IP:   route.IP,
		Port: serverPort,
	}

	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		return
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	clientIP := localAddr.IP.String()

	helloPayload := []byte(fmt.Sprintf(
		"HELLO client_ip=%s client_version=%s",
		clientIP, Version,
	))

	// Send initial HELLO
	_, _ = conn.Write(helloPayload)

	// Keepalive HELLOs
	go func() {
		ticker := time.NewTicker(time.Duration(helloInterval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = conn.Write(helloPayload)
			}
		}
	}()

	// Receive loop
	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			time.Sleep(time.Second)
			continue
		}

		payload := buf[:n]

		// HEARTBEAT detection
		if bytes.HasPrefix(payload, []byte("HEARTBEAT")) {
			markHeartbeat(route.ID)
			continue
		}

		// Discovery packet
		markDiscovery(route.ID)
		rebroadcastDiscoveryPacket(payload)
	}
}

// --- Orchestration: discover routes & start goroutines ---

func startFlexClient(ctx context.Context) {
	routesFound, err := discoverFlextoolRoutes()
	if err != nil {
		return
	}
	if len(routesFound) == 0 {
		return
	}

	initRouteStatusFor(routesFound)

	routesMu.Lock()
	routes = routesFound
	routesMu.Unlock()

	for _, r := range routesFound {
		route := r
		go runForServer(ctx, route)
	}

	// Block until stopped, then clear state
	<-ctx.Done()

	routesMu.Lock()
	routes = nil
	routesMu.Unlock()

	statusMu.Lock()
	routeStatusMap = make(map[string]*routeStatus)
	statusMu.Unlock()
}

// --- Fyne GUI ---

func main() {
	a := app.New()
	w := a.NewWindow("FlexClient GUI")
	w.Resize(fyne.NewSize(600, 400))

	// Route list
	routeList := widget.NewList(
		func() int {
			routesMu.RLock()
			defer routesMu.RUnlock()
			return len(routes)
		},
		func() fyne.CanvasObject {
			return widget.NewLabel("")
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			label := o.(*widget.Label)

			routesMu.RLock()
			defer routesMu.RUnlock()
			if i < 0 || i >= len(routes) {
				label.SetText("")
				return
			}
			r := routes[i]

			hbAgo, rxAgo, hasHB, hasRX := getRouteStatus(r.ID)

			hbText := "HB: none"
			if hasHB {
				hbText = fmt.Sprintf("HB: %s ago", hbAgo.Round(time.Second))
			}

			rxText := "RX: idle"
			if hasRX && rxAgo < discoveryActiveFor {
				rxText = "RX: active"
			} else if hasRX {
				rxText = fmt.Sprintf("RX: idle (%s ago)", rxAgo.Round(time.Second))
			}

			label.SetText(fmt.Sprintf("%s (%s) – %s – %s", r.ID, r.IP.String(), hbText, rxText))
		},
	)

	// Periodic refresh of routeList (for heartbeat/discovery age)
	go func() {
		ticker := time.NewTicker(heartbeatListUpdate)
		defer ticker.Stop()
		for range ticker.C {
			fyne.Do(func() {
				routeList.Refresh()
			})
		}
	}()

	// Start / Stop buttons
	startBtn := widget.NewButton("Start", nil)
	stopBtn := widget.NewButton("Stop", nil)
	stopBtn.Disable()

	startBtn.OnTapped = func() {
		clientMu.Lock()
		defer clientMu.Unlock()

		if clientRunning {
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		clientCancel = cancel
		clientRunning = true

		startBtn.Disable()
		stopBtn.Enable()

		go startFlexClient(ctx)
	}

	stopBtn.OnTapped = func() {
		clientMu.Lock()
		defer clientMu.Unlock()

		if !clientRunning {
			return
		}

		clientCancel()
		clientCancel = nil
		clientRunning = false

		startBtn.Enable()
		stopBtn.Disable()
	}

	// Layout: just buttons + list
	topBar := container.NewHBox(startBtn, stopBtn)
	content := container.NewBorder(topBar, nil, nil, nil, routeList)
	w.SetContent(content)

	w.ShowAndRun()
}
