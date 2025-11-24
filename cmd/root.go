/*
Copyright © 2023 Blair Gillam <ns1h@airmada.net>
Modified by Steven Griggs KC4CAW
*/
package cmd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

const (
	serverPort    = 14992 // UDP port on each flextool server
	defaultNBCLI  = "netbird"
	defaultHelloS = 30
	defaultBcast  = 4992
)

var (
	broadcastPort int
	helloInterval int
	enableDebug   bool
)

// heartbeat tracking: last heartbeat time per Flextool route
var (
	heartbeatMu   sync.RWMutex
	heartbeatLast = make(map[string]time.Time) // key: route.ID
)

// Optional helper kept around if you want it later.
func cidrRangeContains(cidrRange string, checkIP string) (bool, error) {
	_, ipnet, err := net.ParseCIDR(cidrRange)
	if err != nil {
		return false, err
	}
	secondIP := net.ParseIP(checkIP)
	return ipnet.Contains(secondIP), err
}

type flextoolRoute struct {
	ID string
	IP net.IP
}

// pick netbird CLI path from env or default
func netbirdCLIPath() string {
	if p := os.Getenv("NETBIRD_CLI"); p != "" {
		return p
	}
	return defaultNBCLI
}

// discoverFlextoolRoutes runs `netbird routes list` and finds any route whose ID contains "Flextool"
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

		// Look for "ID: ..."
		if strings.Contains(line, "ID:") {
			idx := strings.Index(line, "ID:")
			if idx >= 0 {
				currentID = strings.TrimSpace(line[idx+len("ID:"):])
				isFlextool = strings.Contains(strings.ToLower(currentID), "flextool")
			}
			continue
		}

		// If this route is a Flextool route, look for "Network: x.x.x.x/32"
		if isFlextool && strings.Contains(line, "Network:") {
			idx := strings.Index(line, "Network:")
			if idx < 0 {
				continue
			}
			netStr := strings.TrimSpace(line[idx+len("Network:"):]) // "10.10.2.5/32"
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

			// After we got the network for this Flextool ID, wait for next ID
			isFlextool = false
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error parsing 'netbird routes list': %w", err)
	}

	return routes, nil
}

// handlePacket rebroadcasts UDP payload on the local LAN (FlexRadio discovery)
func handlePacket(buf []byte) {
	dest := &net.UDPAddr{
		IP:   net.IPv4bcast, // 255.255.255.255
		Port: broadcastPort, // usually 4992
	}

	conn, err := net.DialUDP("udp", nil, dest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error dialing broadcast address: %v\n", err)
		return
	}
	defer conn.Close()

	if _, err := conn.Write(buf); err != nil {
		fmt.Fprintf(os.Stderr, "error writing broadcast packet: %v\n", err)
		return
	}

	if enableDebug {
		fmt.Printf("Rebroadcast %d bytes to %s\n", len(buf), dest.String())
	}
}

// recordHeartbeat updates the last heartbeat time for a given Flextool route
func recordHeartbeat(routeID string) {
	heartbeatMu.Lock()
	defer heartbeatMu.Unlock()
	heartbeatLast[routeID] = time.Now()
	if enableDebug {
		fmt.Printf("[%s] Heartbeat received at %s\n", routeID, heartbeatLast[routeID].Format(time.RFC3339))
	}
}

// for each Flextool server: maintain HELLOs, receive heartbeats + discovery, rebroadcast discovery
func runForServer(route flextoolRoute) {
	serverAddr := &net.UDPAddr{
		IP:   route.IP,
		Port: serverPort,
	}

	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] failed to dial flextool server %s: %v\n",
			route.ID, serverAddr.String(), err)
		return
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	clientIP := localAddr.IP.String()

	helloPayload := []byte(fmt.Sprintf(
		"HELLO client_ip=%s client_version=%s",
		clientIP, Version,
	))

	if enableDebug {
		fmt.Printf("[%s] Using local addr %s, sending HELLO to %s\n",
			route.ID, localAddr.String(), serverAddr.String())
	}

	// Send one HELLO immediately
	if _, err := conn.Write(helloPayload); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] error sending initial HELLO: %v\n", route.ID, err)
	}

	// HELLO keepalive goroutine
	go func() {
		ticker := time.NewTicker(time.Duration(helloInterval) * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			if _, err := conn.Write(helloPayload); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] error sending HELLO: %v\n", route.ID, err)
			} else if enableDebug {
				fmt.Printf("[%s] Sent HELLO to %s\n", route.ID, serverAddr.String())
			}
		}
	}()

	// Receive loop: everything from this server
	buf := make([]byte, 4096)
	for {
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[%s] error reading from server: %v\n", route.ID, err)
			time.Sleep(time.Second)
			continue
		}

		payload := buf[:n]

		// Check if this is a heartbeat (text starting with "HEARTBEAT")
		if strings.HasPrefix(string(payload), "HEARTBEAT") {
			recordHeartbeat(route.ID)
			// Heartbeats are control messages; don't rebroadcast them
			if enableDebug {
				fmt.Printf("[%s] Heartbeat packet from %s: %q\n", route.ID, addr.String(), string(payload))
			}
			continue
		}

		// Otherwise, assume this is a Flex discovery packet to rebroadcast
		if enableDebug {
			fmt.Printf("[%s] Received %d bytes from %s, rebroadcasting\n", route.ID, n, addr.String())
		}
		handlePacket(payload)
	}
}

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "flexclient",
	Short: "A FlexRadio discovery rebroadcaster for VPN/NetBird clients",
	Long: `flexclient
- Auto-discovers NetBird routes whose ID contains "Flextool"
- Sends HELLO messages (with client IP & version) to each flextool server on UDP 14992
- Receives heartbeats from each server and tracks last heartbeat time
- Receives FlexRadio discovery packets from those servers
- Rebroadcasts discovery packets on the local LAN (255.255.255.255:4992) so SmartSDR sees them`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if helloInterval < 1 {
			return errors.New("hello-interval must be >= 1 second")
		}
		if broadcastPort < 1 || broadcastPort > 65535 {
			return fmt.Errorf("broadcast-port must be between 1 and 65535 (got %d)", broadcastPort)
		}

		routes, err := discoverFlextoolRoutes()
		if err != nil {
			return err
		}
		if len(routes) == 0 {
			return errors.New("no NetBird routes found with 'Flextool' in the ID; check 'netbird routes list'")
		}

		fmt.Printf("Discovered %d Flextool route(s):\n", len(routes))
		for _, r := range routes {
			fmt.Printf("  - %s (%s)\n", r.ID, r.IP.String())
		}

		fmt.Printf("Broadcasting on 255.255.255.255:%d, HELLO interval %ds\n", broadcastPort, helloInterval)

		// Start a goroutine per server
		for _, r := range routes {
			go runForServer(r)
		}

		fmt.Println("flexclient running. Press Ctrl+C to stop.")
		// Block forever; goroutines do the work
		select {}
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
	rootCmd.Flags().BoolVarP(&enableDebug, "debug", "d", false, "Turns on verbose debug output to the console")

	rootCmd.Flags().IntVar(&broadcastPort, "broadcast-port", defaultBcast, "LAN broadcast port for FlexRadio discovery")
	rootCmd.Flags().IntVar(&helloInterval, "hello-interval", defaultHelloS, "Seconds between HELLO keepalive messages to each flextool server")
}
