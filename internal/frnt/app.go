//go:build windows || darwin

package frnt

import (
	"bufio"
	"context"
	"fmt"
	"image/color"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexclient"
	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/procutil"
)

const (
	AppName              = "Flex Radio Network Tool"
	Version              = "0.1.2" // bump as needed
	heartbeatListUpdate  = 1 * time.Second
	discoveryActiveFor   = 10 * time.Second // RX "active" window
	netbirdStatusTimeout = 5 * time.Second

	// fallback SmartSDR version (used only if winget detection fails)
	SmartSDRVersionFallback = "Unknown"
)

// --- logging setup ---

func setupLogging() {
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("os.Executable error: %v", err)
		return
	}
	dir := filepath.Dir(exePath)
	logPath := filepath.Join(dir, "flexclient-gui.log") // rename if you like

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("failed to open log file %s: %v", logPath, err)
		return
	}

	log.SetOutput(f)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("===== %s v%s started =====", AppName, Version)
	log.Printf("Executable: %s", exePath)

	logGPUInfo()
}

// GPU info (helps debug OpenGL issues)
func logGPUInfo() {
	if runtime.GOOS != "windows" {
		log.Printf("GPU info: non-Windows OS (%s), skipping GPU query", runtime.GOOS)
		return
	}

	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		`Get-CimInstance Win32_VideoController | Select-Object -ExpandProperty Name`)
	procutil.HideWindow(cmd)

	out, err := cmd.Output()
	if err != nil {
		log.Printf("GPU info: failed to query via PowerShell: %v", err)
		return
	}

	lines := strings.Split(string(out), "\n")
	var gpus []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			gpus = append(gpus, l)
		}
	}

	if len(gpus) == 0 {
		log.Printf("GPU info: no adapters found")
		return
	}

	for i, g := range gpus {
		log.Printf("GPU[%d]: %s", i, g)
	}
}

// --- NetBird + SmartSDR version helpers for About page ---

func netbirdCLIPath() string {
	if p := os.Getenv("NETBIRD_CLI"); p != "" {
		return p
	}
	return "netbird"
}

// getNetbirdVersions runs "netbird status" and parses Daemon/CLI versions.
func getNetbirdVersions() (daemonVer, cliVer string, err error) {
	cmdPath := netbirdCLIPath()
	cmd := exec.Command(cmdPath, "status")
	procutil.HideWindow(cmd)

	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		return "", "", fmt.Errorf("netbird status error: %w (output: %s)", err, output)
	}

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Daemon version:") {
			daemonVer = strings.TrimSpace(strings.TrimPrefix(line, "Daemon version:"))
		} else if strings.HasPrefix(line, "CLI version:") {
			cliVer = strings.TrimSpace(strings.TrimPrefix(line, "CLI version:"))
		}
	}
	if err := scanner.Err(); err != nil {
		return daemonVer, cliVer, fmt.Errorf("scanner error: %w", err)
	}

	return daemonVer, cliVer, nil
}

// getSmartSDRVersion runs "winget list" and tries to find the SmartSDR entry.
// Uses a timeout so it can't hang the app.
func getSmartSDRVersion() (string, error) {
	if runtime.GOOS != "windows" {
		return "", fmt.Errorf("SmartSDR detection only implemented on Windows")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "winget", "list")
	procutil.HideWindow(cmd)

	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("winget list timed out")
	}
	if err != nil {
		return "", fmt.Errorf("winget list error: %w (output: %s)", err, string(out))
	}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.Contains(line, "SmartSDR") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				ver := fields[len(fields)-1]
				return ver, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scanner error: %w", err)
	}

	return "", fmt.Errorf("SmartSDR not found in winget list output")
}

// --- GUI entrypoint ---

func Run() {
	setupLogging()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC: %v", r)
		}
		log.Printf("===== %s exiting =====", AppName)
	}()

	log.Printf("GUI: initializing Fyne app")
	a := app.New()
	w := a.NewWindow(AppName)
	w.Resize(fyne.NewSize(800, 450))
	log.Printf("GUI: window created")

	// ---------- Shared state for flexclient start/stop ----------
	var (
		clientMu       sync.Mutex
		clientCancel   context.CancelFunc
		clientStarting bool
		clientRunning  bool
	)
	var firewallCheckGeneration uint64
	adminUnlocked := false
	var aboutSelectTapCount int
	var aboutSelectLastTap time.Time
	menuItems := []string{"Flexclient", "Settings", "Help", "About"}
	var menu *widget.List
	var refreshMenuItems func()

	// Load saved proxy/direct settings if present.
	if persisted, err := loadProxySettingsFromDisk(); err == nil {
		flexclient.SetRadioModeSettings(persisted.ProxyBasePort, persisted.RadioModes)
		ignoredRoutes := map[string]bool{}
		for _, routeID := range persisted.IgnoredRoutes {
			routeID = strings.TrimSpace(routeID)
			if routeID == "" {
				continue
			}
			ignoredRoutes[routeID] = true
		}
		flexclient.SetIgnoredRoutes(ignoredRoutes)
		log.Printf("GUI: loaded proxy settings from %s", clientSettingsPath())
	} else {
		log.Printf("GUI: proxy settings file not loaded (%v), using env/defaults", err)
	}

	persistRuntimeSettings := func() error {
		basePort, modes := flexclient.GetRadioModeSettings()
		ignoredMap := flexclient.GetIgnoredRoutes()
		ignoredRoutes := make([]string, 0, len(ignoredMap))
		for routeID, ignored := range ignoredMap {
			if ignored {
				ignoredRoutes = append(ignoredRoutes, routeID)
			}
		}
		return saveProxySettingsToDisk(proxySettingsFile{
			ProxyBasePort: basePort,
			RadioModes:    modes,
			IgnoredRoutes: ignoredRoutes,
		})
	}

	// ---------- Flexclient page: expandable route cards + Start/Stop ----------
	routeExpanded := map[string]bool{}
	routeCards := container.NewVBox()
	routeCardsScroll := container.NewVScroll(routeCards)

	var rebuildRouteCards func()
	rebuildRouteCards = func() {
		rs := flexclient.Routes()
		rows := make([]fyne.CanvasObject, 0, len(rs))

		for _, route := range rs {
			r := route
			if _, ok := routeExpanded[r.ID]; !ok {
				routeExpanded[r.ID] = false
			}

			hbAgo, rxAgo, hasHB, hasRX := flexclient.GetRouteStatus(r.ID)
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

			title := widget.NewLabel(fmt.Sprintf("%s (%s) - %s - %s", r.ID, r.IP.String(), hbText, rxText))

			toggleText := "Show Radios"
			if routeExpanded[r.ID] {
				toggleText = "Hide Radios"
			}
			routeID := r.ID
			toggleBtn := widget.NewButton(toggleText, func() {
				routeExpanded[routeID] = !routeExpanded[routeID]
				rebuildRouteCards()
			})

			ignoreRouteCheck := widget.NewCheck("Ignore FlexTool", nil)
			ignoreRouteCheck.SetChecked(flexclient.IsRouteIgnored(r.ID))
			ignoreRouteCheck.OnChanged = func(checked bool) {
				flexclient.SetRouteIgnored(routeID, checked)
				if err := persistRuntimeSettings(); err != nil {
					log.Printf("GUI: failed to persist route ignore change for %s: %v", routeID, err)
					return
				}
				log.Printf("GUI: set route %s ignored=%v", routeID, checked)
			}

			headerRow := container.NewHBox(toggleBtn, title, layout.NewSpacer(), ignoreRouteCheck)
			cardBody := []fyne.CanvasObject{headerRow}

			if routeExpanded[r.ID] {
				radios := flexclient.GetRouteRadioStatuses(r.ID)
				if len(radios) == 0 {
					cardBody = append(cardBody, widget.NewLabel("No radios seen yet."))
				} else {
					for _, radio := range radios {
						radioState := "idle"
						age := time.Since(radio.LastSeen).Round(time.Second)
						if time.Since(radio.LastSeen) < discoveryActiveFor {
							radioState = "active"
						}

						radioLabel := widget.NewLabel(
							fmt.Sprintf("Radio %s - %s (%s ago, packets=%d)", radio.Serial, radioState, age, radio.PacketSeen),
						)

						modeRadio := widget.NewRadioGroup([]string{"direct", "proxy", "off"}, nil)
						modeRadio.Horizontal = true
						modeRadio.SetSelected(flexclient.GetRadioMode(radio.Serial))

						serial := radio.Serial
						modeRadio.OnChanged = func(mode string) {
							if mode == "" {
								return
							}
							flexclient.SetRadioModeForSerial(serial, mode)
							if err := persistRuntimeSettings(); err != nil {
								log.Printf("GUI: failed to persist per-radio mode change for %s: %v", serial, err)
								return
							}
							log.Printf("GUI: set radio %s mode=%s", serial, mode)
						}

						radioRow := container.NewHBox(
							layout.NewSpacer(),
							radioLabel,
							layout.NewSpacer(),
							widget.NewLabel("Mode"),
							modeRadio,
						)
						cardBody = append(cardBody, radioRow)
					}
				}
			}

			card := widget.NewCard("", "", container.NewVBox(cardBody...))
			rows = append(rows, card)
		}

		routeCards.Objects = rows
		routeCards.Refresh()
	}

	rebuildRouteCards()

	// Periodic refresh of route cards.
	go func() {
		log.Printf("GUI: starting route card refresh ticker")
		ticker := time.NewTicker(heartbeatListUpdate)
		defer ticker.Stop()
		for range ticker.C {
			fyne.Do(func() {
				rebuildRouteCards()
			})
		}
	}()

	startBtn := widget.NewButton("Start", nil)
	stopBtn := widget.NewButton("Stop", nil)
	stopBtn.Disable()

	setStoppedUI := func() {
		startBtn.SetText("Start")
		startBtn.Enable()
		stopBtn.Disable()
	}

	startBtn.OnTapped = func() {
		log.Printf("GUI: Start clicked")

		clientMu.Lock()
		if clientRunning || clientStarting {
			log.Printf("GUI: client already running/starting, ignoring Start")
			clientMu.Unlock()
			return
		}
		clientStarting = true
		clientMu.Unlock()

		startBtn.Disable()
		startBtn.SetText("Starting...")
		stopBtn.Disable()

		go func() {
			// Check NetBird status before starting flexclient.
			connected, needsLogin, raw, err := flexclient.CheckNetbirdStatus(netbirdStatusTimeout)
			if err != nil {
				log.Printf("GUI: NetBird status check failed: %v, output:\n%s", err, raw)
				fyne.Do(func() {
					clientMu.Lock()
					clientStarting = false
					clientMu.Unlock()
					setStoppedUI()
					dialog.ShowInformation(
						"NetBird status",
						"Could not check NetBird status. Please make sure NetBird is installed, running, and connected, then try again.",
						w,
					)
				})
				return
			}
			if !connected {
				if needsLogin {
					log.Printf("GUI: NetBird requires login (NeedsLogin)")
				} else {
					log.Printf("GUI: NetBird not connected to management")
				}

				fyne.Do(func() {
					clientMu.Lock()
					clientStarting = false
					clientMu.Unlock()
					setStoppedUI()
					dialog.ShowInformation(
						"NetBird not connected",
						"Please log into NetBird then try again.",
						w,
					)
				})
				return
			}

			ctx, cancel := context.WithCancel(context.Background())
			startupResult := make(chan error, 1)
			go flexclient.Start(ctx, Version, startupResult)

			startErr := <-startupResult
			if startErr != nil {
				log.Printf("GUI: flexclient failed to start: %v", startErr)
				cancel()

				fyne.Do(func() {
					clientMu.Lock()
					clientCancel = nil
					clientRunning = false
					clientStarting = false
					clientMu.Unlock()
					setStoppedUI()
					dialog.ShowInformation(
						"Flexclient start failed",
						fmt.Sprintf("Could not start flexclient:\n\n%v", startErr),
						w,
					)
				})
				return
			}

			fyne.Do(func() {
				clientMu.Lock()
				clientCancel = cancel
				clientRunning = true
				clientStarting = false
				clientMu.Unlock()

				startBtn.SetText("Start")
				startBtn.Disable()
				stopBtn.Enable()
			})
		}()
	}

	stopBtn.OnTapped = func() {
		log.Printf("GUI: Stop clicked")
		clientMu.Lock()
		cancel := clientCancel
		if !clientRunning {
			log.Printf("GUI: client not running, ignoring Stop")
			clientMu.Unlock()
			return
		}

		clientCancel = nil
		clientRunning = false
		clientStarting = false
		clientMu.Unlock()

		if cancel != nil {
			cancel()
		}

		setStoppedUI()
	}

	flexclientTopBar := container.NewHBox(startBtn, stopBtn)
	flexclientPage := container.NewBorder(flexclientTopBar, nil, nil, nil, routeCardsScroll)

	// ---------- Settings page ----------
	proxyBasePort, radioModes := flexclient.GetRadioModeSettings()

	proxyBasePortEntry := widget.NewEntry()
	proxyBasePortEntry.SetText(fmt.Sprintf("%d", proxyBasePort))
	proxyBasePortEntry.SetPlaceHolder("30000")

	radioModesEntry := widget.NewMultiLineEntry()
	radioModesEntry.SetPlaceHolder("serial=proxy\nanother-serial=direct\nthird-serial=off")
	radioModesEntry.SetMinRowsVisible(14)
	radioModesEntry.SetText(formatRadioModesText(radioModes))

	settingsStatus := widget.NewLabel("Edit settings and click Save + Apply.")

	saveApplyBtn := widget.NewButton("Save + Apply", func() {
		basePort, err := parseProxyBasePort(proxyBasePortEntry.Text)
		if err != nil {
			settingsStatus.SetText("Error: " + err.Error())
			dialog.ShowError(err, w)
			return
		}

		modes, err := parseRadioModesText(radioModesEntry.Text)
		if err != nil {
			settingsStatus.SetText("Error: " + err.Error())
			dialog.ShowError(err, w)
			return
		}

		flexclient.SetRadioModeSettings(basePort, modes)
		if err := persistRuntimeSettings(); err != nil {
			settingsStatus.SetText("Applied in memory, but save failed: " + err.Error())
			dialog.ShowError(err, w)
			return
		}

		settingsStatus.SetText("Saved and applied.")
		log.Printf("GUI: proxy settings saved/applied (%d radios)", len(modes))
	})

	reloadBtn := widget.NewButton("Reload Saved", func() {
		cfg, err := loadProxySettingsFromDisk()
		if err != nil {
			settingsStatus.SetText("Reload failed: " + err.Error())
			dialog.ShowError(err, w)
			return
		}

		flexclient.SetRadioModeSettings(cfg.ProxyBasePort, cfg.RadioModes)
		ignoredRoutes := map[string]bool{}
		for _, routeID := range cfg.IgnoredRoutes {
			routeID = strings.TrimSpace(routeID)
			if routeID == "" {
				continue
			}
			ignoredRoutes[routeID] = true
		}
		flexclient.SetIgnoredRoutes(ignoredRoutes)
		proxyBasePortEntry.SetText(fmt.Sprintf("%d", cfg.ProxyBasePort))
		radioModesEntry.SetText(formatRadioModesText(cfg.RadioModes))
		settingsStatus.SetText("Reloaded saved settings.")
	})

	readRuntimeBtn := widget.NewButton("Load Current Runtime", func() {
		base, modes := flexclient.GetRadioModeSettings()
		proxyBasePortEntry.SetText(fmt.Sprintf("%d", base))
		radioModesEntry.SetText(formatRadioModesText(modes))
		settingsStatus.SetText("Loaded current runtime settings.")
	})

	settingsForm := widget.NewForm(
		widget.NewFormItem("Proxy Base Port", proxyBasePortEntry),
	)

	settingsHelp := widget.NewLabel(
		"Per radio mode (one per line): serial=direct, serial=proxy, or serial=off.\n" +
			"Modes are explicit only. Unlisted radios stay direct.",
	)

	settingsPage := container.NewBorder(
		container.NewVBox(
			settingsForm,
			settingsHelp,
			container.NewHBox(saveApplyBtn, reloadBtn, readRuntimeBtn),
			settingsStatus,
			widget.NewSeparator(),
		),
		nil,
		nil,
		nil,
		radioModesEntry,
	)

	// ---------- About page ----------
	appVersionLabel := widget.NewLabel(AppName + " Version: " + Version)

	netbirdVersionLabel := widget.NewLabel("NetBird: detecting...")
	smartSDRLabel := widget.NewLabel("SmartSDR Version: detecting...")

	firewallLabel := widget.NewLabel("Firewall: checking...")
	firewallFixBtn := widget.NewButton("Fix Firewall Rule", nil)
	firewallFixBtn.Disable()

	aboutText := widget.NewLabel(
		"Flex Radio Network Tool\n\n" +
			"Internal engine: flexclient\n\n" +
			"This tool discovers FlexRadio broadcasts across NetBird VPN\n" +
			"and rebroadcasts them on your local network so SmartSDR\n" +
			"and other clients can see your radios as if they were local.",
	)

	aboutHeader := widget.NewLabelWithStyle("About", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

	aboutPage := container.NewVBox(
		aboutHeader,
		appVersionLabel,
		netbirdVersionLabel,
		smartSDRLabel,
		firewallLabel,
		firewallFixBtn,
		widget.NewSeparator(),
		aboutText,
	)

	// ---------- Help page ----------
	helpHeader := widget.NewLabelWithStyle("Help", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	helpText := widget.NewLabel(
		"What This Tool Does\n" +
			"- Client mode discovers FRNT servers over NetBird and rebroadcasts Flex discovery locally for SmartSDR.\n" +
			"- Server mode (Linux/headless) listens for radios on LAN and serves discovery/control/stream traffic to clients.\n\n" +
			"Flexclient Page\n" +
			"- Start: connects to Flextool routes and begins rebroadcasting discoveries.\n" +
			"- Stop: disconnects all route workers.\n" +
			"- Show Radios: expands a route card to list radios seen on that route.\n" +
			"- Ignore FlexTool: hides all radios from that route from local discovery.\n\n" +
			"Per-Radio Mode\n" +
			"- direct: SmartSDR connects to the discovered radio endpoint directly.\n" +
			"- proxy: discovery is rewritten so SmartSDR connects through the FRNT server.\n" +
			"- off: this radio is ignored and not shown to SmartSDR.\n\n" +
			"Settings Page\n" +
			"- Proxy Base Port: base used for per-radio proxy listeners.\n" +
			"- Radio Modes: optional serial=mode overrides (direct/proxy/off).\n" +
			"- Save + Apply writes settings to flexclient-settings.json and applies immediately.\n\n" +
			"Quick Troubleshooting\n" +
			"- If Start fails, check NetBird login/management connectivity.\n" +
			"- If radios are missing, verify route is not ignored and radio mode is not off.\n" +
			"- If proxy fails, confirm server service is active and firewall rule is OK on About page.\n" +
			"- Logs are written to flexclient-gui.log next to frnt.exe.",
	)
	helpText.Wrapping = fyne.TextWrapWord
	helpPage := container.NewBorder(
		container.NewVBox(helpHeader, widget.NewSeparator()),
		nil,
		nil,
		nil,
		container.NewVScroll(helpText),
	)

	// ---------- Hidden Admin page ----------
	defaultListenInterface := strings.TrimSpace(os.Getenv("LISTEN_INTERFACE"))
	if defaultListenInterface == "" {
		defaultListenInterface = "ens18"
	}
	defaultSendInterface := strings.TrimSpace(os.Getenv("SEND_INTERFACE"))
	if defaultSendInterface == "" {
		defaultSendInterface = "ens18"
	}
	defaultAPIURL := strings.TrimSpace(os.Getenv("NETBIRD_API_URL"))
	if defaultAPIURL == "" {
		defaultAPIURL = "https://netbird.w4car.org/api/peers"
	}

	adminHeader := widget.NewLabelWithStyle("Admin (Hidden)", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	adminHint := widget.NewLabel("Batch tools for Flextool servers. Use carefully.")
	adminStatus := widget.NewLabel("Idle.")
	adminStatus.Wrapping = fyne.TextWrapWord

	adminServersEntry := widget.NewMultiLineEntry()
	adminServersEntry.SetPlaceHolder("one per line:\nuser@10.2.0.164\nuser@10.10.1.50:22")
	adminServersEntry.SetMinRowsVisible(7)

	adminDefaultUser := widget.NewEntry()
	adminDefaultUser.SetText("root")

	adminSSHPassword := widget.NewPasswordEntry()
	adminSSHPassword.SetPlaceHolder("SSH password (optional if keys are available)")

	adminSudoPassword := widget.NewPasswordEntry()
	adminSudoPassword.SetPlaceHolder("Sudo password (blank = same as SSH password)")

	adminInstallCommand := widget.NewMultiLineEntry()
	adminInstallCommand.SetPlaceHolder("Optional mass install/build command.\nExample:\ncd /home/testbed/frnt-smoke/src && go build -o /home/testbed/frnt-smoke/frnt .")
	adminInstallCommand.SetMinRowsVisible(4)

	adminServiceName := widget.NewEntry()
	adminServiceName.SetText("frnt-listen.service")

	adminBinaryPath := widget.NewEntry()
	adminBinaryPath.SetText("/usr/local/bin/frnt")

	adminRepoURL := widget.NewEntry()
	adminRepoURL.SetText("https://github.com/KingSteve032/Flex-Radio-Network-Tool.git")

	adminSourceDir := widget.NewEntry()
	adminSourceDir.SetText("/opt/frnt/src")

	adminBuildOutput := widget.NewEntry()
	adminBuildOutput.SetText("/opt/frnt/frnt")

	adminBroadcast := widget.NewCheck("", nil)
	adminBroadcast.SetChecked(true)

	adminListenIF := widget.NewEntry()
	adminListenIF.SetText(defaultListenInterface)

	adminSendIF := widget.NewEntry()
	adminSendIF.SetText(defaultSendInterface)

	adminDebug := widget.NewCheck("", nil)
	adminDebug.SetChecked(false)

	adminAPIToken := widget.NewPasswordEntry()
	adminAPIToken.SetText(strings.TrimSpace(os.Getenv("NETBIRD_API_TOKEN")))

	adminAPIURL := widget.NewEntry()
	adminAPIURL.SetText(defaultAPIURL)

	adminDiscoveryDelay := widget.NewEntry()
	adminDiscoveryDelay.SetText("15")

	adminSyncInterval := widget.NewEntry()
	adminSyncInterval.SetText("60")

	adminIgnoreRadios := widget.NewEntry()
	adminIgnoreRadios.SetPlaceHolder("comma-separated IPs (optional)")

	adminEnableVita := widget.NewCheck("", nil)
	adminEnableVita.SetChecked(true)

	adminVitaPort := widget.NewEntry()
	adminVitaPort.SetText("4991")

	adminProxyBasePort := widget.NewEntry()
	adminProxyBasePort.SetText("30000")

	adminMultiProxy := widget.NewCheck("", nil)
	adminMultiProxy.SetChecked(true)

	adminLog := widget.NewMultiLineEntry()
	adminLog.SetMinRowsVisible(12)
	adminLog.SetPlaceHolder("Batch output will appear here...")

	appendAdminLog := func(line string) {
		fyne.Do(func() {
			ts := time.Now().Format("15:04:05")
			if strings.TrimSpace(adminLog.Text) == "" {
				adminLog.SetText("[" + ts + "] " + line)
			} else {
				adminLog.SetText(adminLog.Text + "\n[" + ts + "] " + line)
			}
		})
	}

	parsePositiveIntField := func(name, raw string) (int, error) {
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || v < 0 {
			return 0, fmt.Errorf("%s must be a non-negative integer", name)
		}
		return v, nil
	}

	runAdminAction := func(applyConfig, runInstall, installService, restartService bool, installCommandOverride string) {
		targets, err := parseAdminTargets(adminServersEntry.Text, adminDefaultUser.Text)
		if err != nil {
			dialog.ShowError(err, w)
			adminStatus.SetText("Input error: " + err.Error())
			return
		}

		discoveryDelay, err := parsePositiveIntField("Discovery Delay", adminDiscoveryDelay.Text)
		if err != nil {
			dialog.ShowError(err, w)
			adminStatus.SetText("Input error: " + err.Error())
			return
		}
		syncInterval, err := parsePositiveIntField("Sync Interval", adminSyncInterval.Text)
		if err != nil {
			dialog.ShowError(err, w)
			adminStatus.SetText("Input error: " + err.Error())
			return
		}
		vitaPort, err := parsePositiveIntField("VITA Proxy Port", adminVitaPort.Text)
		if err != nil {
			dialog.ShowError(err, w)
			adminStatus.SetText("Input error: " + err.Error())
			return
		}
		proxyBase, err := parsePositiveIntField("Proxy Base Port", adminProxyBasePort.Text)
		if err != nil {
			dialog.ShowError(err, w)
			adminStatus.SetText("Input error: " + err.Error())
			return
		}

		installCommand := strings.TrimSpace(adminInstallCommand.Text)
		if strings.TrimSpace(installCommandOverride) != "" {
			installCommand = strings.TrimSpace(installCommandOverride)
		}

		req := adminBatchRequest{
			Targets:        targets,
			SSHPassword:    adminSSHPassword.Text,
			SudoPassword:   adminSudoPassword.Text,
			InstallCommand: installCommand,
			ServiceName:    strings.TrimSpace(adminServiceName.Text),
			BinaryPath:     strings.TrimSpace(adminBinaryPath.Text),
			FlexToolConfig: adminFlexToolConfig{
				Broadcast:             adminBroadcast.Checked,
				ListenInterface:       strings.TrimSpace(adminListenIF.Text),
				SendInterface:         strings.TrimSpace(adminSendIF.Text),
				Debug:                 adminDebug.Checked,
				NetBirdAPIToken:       strings.TrimSpace(adminAPIToken.Text),
				NetBirdAPIURL:         strings.TrimSpace(adminAPIURL.Text),
				DiscoveryDelaySeconds: discoveryDelay,
				SyncIntervalSeconds:   syncInterval,
				IgnoreRadios:          strings.TrimSpace(adminIgnoreRadios.Text),
				EnableVitaProxy:       adminEnableVita.Checked,
				VitaProxyPort:         vitaPort,
				ProxyBasePort:         proxyBase,
				MultiProxy:            adminMultiProxy.Checked,
			},
			ApplyConfig:    applyConfig,
			InstallService: installService,
			RestartService: restartService,
			RunInstallCmd:  runInstall && installCommand != "",
		}

		adminStatus.SetText("Running...")
		appendAdminLog("Starting batch run...")
		go func() {
			okCount, failCount := runAdminBatch(req, appendAdminLog)
			fyne.Do(func() {
				adminStatus.SetText(fmt.Sprintf("Done. Success=%d Failed=%d", okCount, failCount))
			})
		}()
	}

	adminApplyConfigBtn := widget.NewButton("Apply Config To All", func() {
		runAdminAction(true, false, false, false, "")
	})
	adminInstallCmdBtn := widget.NewButton("Run Install Cmd On All", func() {
		runAdminAction(false, true, false, false, "")
	})
	adminInstallSvcBtn := widget.NewButton("Install/Repair Service", func() {
		runAdminAction(false, false, true, false, "")
	})
	adminRestartSvcBtn := widget.NewButton("Restart Service", func() {
		runAdminAction(false, false, false, true, "")
	})
	adminFullBtn := widget.NewButton("Full Apply (Install Cmd + Config + Service + Restart)", func() {
		runAdminAction(true, true, true, true, "")
	})
	adminAutoBootstrapBtn := widget.NewButton("Auto Bootstrap + Deploy", func() {
		cmd := buildAutoBootstrapCommand(
			adminRepoURL.Text,
			adminSourceDir.Text,
			adminBuildOutput.Text,
			adminBinaryPath.Text,
			func() string {
				if strings.TrimSpace(adminSudoPassword.Text) != "" {
					return adminSudoPassword.Text
				}
				return adminSSHPassword.Text
			}(),
		)
		runAdminAction(true, true, true, true, cmd)
	})

	adminConfigForm := widget.NewForm(
		widget.NewFormItem("Servers", adminServersEntry),
		widget.NewFormItem("Default SSH User", adminDefaultUser),
		widget.NewFormItem("SSH Password", adminSSHPassword),
		widget.NewFormItem("Sudo Password", adminSudoPassword),
		widget.NewFormItem("Repo URL", adminRepoURL),
		widget.NewFormItem("Source Dir", adminSourceDir),
		widget.NewFormItem("Build Output", adminBuildOutput),
		widget.NewFormItem("Install Command", adminInstallCommand),
		widget.NewFormItem("Service Name", adminServiceName),
		widget.NewFormItem("Server Binary Path", adminBinaryPath),
		widget.NewFormItem("BROADCAST", adminBroadcast),
		widget.NewFormItem("LISTEN_INTERFACE", adminListenIF),
		widget.NewFormItem("SEND_INTERFACE", adminSendIF),
		widget.NewFormItem("DEBUG", adminDebug),
		widget.NewFormItem("NETBIRD_API_TOKEN", adminAPIToken),
		widget.NewFormItem("NETBIRD_API_URL", adminAPIURL),
		widget.NewFormItem("DISCOVERY_DELAY_SECONDS", adminDiscoveryDelay),
		widget.NewFormItem("SYNC_INTERVAL_SECONDS", adminSyncInterval),
		widget.NewFormItem("IGNORE_RADIOS", adminIgnoreRadios),
		widget.NewFormItem("ENABLE_VITA_PROXY", adminEnableVita),
		widget.NewFormItem("VITA_PROXY_PORT", adminVitaPort),
		widget.NewFormItem("PROXY_BASE_PORT", adminProxyBasePort),
		widget.NewFormItem("MULTI_PROXY", adminMultiProxy),
	)

	adminPage := container.NewBorder(
		container.NewVBox(
			adminHeader,
			adminHint,
			adminStatus,
			container.NewHBox(adminAutoBootstrapBtn),
			container.NewHBox(adminApplyConfigBtn, adminInstallCmdBtn, adminInstallSvcBtn),
			container.NewHBox(adminRestartSvcBtn, adminFullBtn),
			widget.NewSeparator(),
		),
		nil,
		nil,
		nil,
		container.NewVScroll(container.NewVBox(adminConfigForm, widget.NewSeparator(), adminLog)),
	)

	// Refresh function so Firewall status never gets "stuck on checking..."
	refreshFirewallStatus := func() {
		checkID := atomic.AddUint64(&firewallCheckGeneration, 1)

		// set visible immediate state
		firewallLabel.SetText("Firewall: checking...")
		firewallFixBtn.Disable()

		go func(id uint64) {
			exePath, err := os.Executable()
			if err != nil {
				fyne.Do(func() {
					if id != atomic.LoadUint64(&firewallCheckGeneration) {
						return
					}
					firewallLabel.SetText("Firewall: unable to detect EXE path")
					firewallFixBtn.Enable()
				})
				return
			}

			chk, err := CheckFirewallRule(exePath)
			fyne.Do(func() {
				if id != atomic.LoadUint64(&firewallCheckGeneration) {
					return
				}
				if err != nil {
					firewallLabel.SetText("Firewall: check failed (click Fix)")
					firewallFixBtn.Enable()
					return
				}
				if !chk.Exists {
					firewallLabel.SetText("Firewall: rule missing")
					firewallFixBtn.Enable()
					return
				}
				if !chk.InboundRuleFound {
					firewallLabel.SetText("Firewall: inbound rule missing")
					firewallFixBtn.Enable()
					return
				}
				if !chk.ProgramMatches {
					firewallLabel.SetText("Firewall: rule points to a different EXE path")
					firewallFixBtn.Enable()
					return
				}
				if !chk.InboundRuleOK {
					firewallLabel.SetText("Firewall: inbound rule needs correction")
					firewallFixBtn.Enable()
					return
				}

				firewallLabel.SetText("Firewall: OK")
				firewallFixBtn.Disable()
			})
		}(checkID)
	}

	// Fix button (UAC prompt happens here ONLY)
	firewallFixBtn.OnTapped = func() {
		atomic.AddUint64(&firewallCheckGeneration, 1)

		exePath, err := os.Executable()
		if err != nil {
			dialog.ShowError(fmt.Errorf("cannot locate current executable: %w", err), w)
			return
		}

		firewallFixBtn.Disable()
		firewallFixBtn.SetText("Fixing...")

		go func() {
			err := EnsureFirewallRule(exePath)

			fyne.Do(func() {
				firewallFixBtn.SetText("Fix Firewall Rule")
				if err != nil {
					firewallLabel.SetText("Firewall: fix failed")
					firewallFixBtn.Enable()
					dialog.ShowError(err, w)
					return
				}

				refreshFirewallStatus()
				dialog.ShowInformation("Firewall", "Firewall rule added/updated successfully.", w)
			})
		}()
	}

	// Resolve NetBird version in background
	go func() {
		daemonVer, cliVer, err := getNetbirdVersions()
		var text string
		if err != nil {
			log.Printf("About: failed to get NetBird version: %v", err)
			text = "NetBird: not detected"
		} else if daemonVer == "" && cliVer == "" {
			text = "NetBird: version unknown"
		} else if daemonVer != "" && cliVer != "" {
			text = fmt.Sprintf("NetBird: Daemon %s, CLI %s", daemonVer, cliVer)
		} else if daemonVer != "" {
			text = fmt.Sprintf("NetBird: Daemon %s", daemonVer)
		} else {
			text = fmt.Sprintf("NetBird: CLI %s", cliVer)
		}

		fyne.Do(func() {
			netbirdVersionLabel.SetText(text)
		})
	}()

	// Resolve SmartSDR version in background via winget
	go func() {
		ver, err := getSmartSDRVersion()
		var text string
		if err != nil {
			log.Printf("About: failed to get SmartSDR version: %v", err)
			text = "SmartSDR Version: " + SmartSDRVersionFallback
		} else {
			text = "SmartSDR Version: " + ver
		}
		fyne.Do(func() {
			smartSDRLabel.SetText(text)
		})
	}()

	// ---------- Left menu + content switching ----------
	contentStack := container.NewMax(flexclientPage) // default view

	menu = widget.NewList(
		func() int { return len(menuItems) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			o.(*widget.Label).SetText(menuItems[i])
		},
	)

	refreshMenuItems = func() {
		base := []string{"Flexclient", "Settings", "Help", "About"}
		if adminUnlocked {
			base = append(base, "Admin")
		}
		menuItems = base
		menu.Refresh()
	}
	refreshMenuItems()

	menu.OnSelected = func(id widget.ListItemID) {
		if id < 0 || int(id) >= len(menuItems) {
			return
		}
		switch menuItems[id] {
		case "Flexclient":
			contentStack.Objects = []fyne.CanvasObject{flexclientPage}
		case "Settings":
			contentStack.Objects = []fyne.CanvasObject{settingsPage}
		case "Help":
			contentStack.Objects = []fyne.CanvasObject{helpPage}
		case "Admin":
			contentStack.Objects = []fyne.CanvasObject{adminPage}
		case "About":
			contentStack.Objects = []fyne.CanvasObject{aboutPage}
			// Update firewall status whenever About is opened
			refreshFirewallStatus()
			// Hidden admin unlock: select About repeatedly within a short window.
			now := time.Now()
			if now.Sub(aboutSelectLastTap) <= 5*time.Second {
				aboutSelectTapCount++
			} else {
				aboutSelectTapCount = 1
			}
			aboutSelectLastTap = now
			if !adminUnlocked && aboutSelectTapCount >= 9 {
				adminUnlocked = true
				log.Printf("GUI: hidden admin page unlocked via About multi-select")
				if refreshMenuItems != nil {
					refreshMenuItems()
				}
				dialog.ShowInformation("Advanced", "Advanced tools unlocked.", w)
			}
			// Allow repeated About clicks to trigger OnSelected again without
			// requiring the user to switch to another menu item first.
			menu.Unselect(id)
		}
		contentStack.Refresh()
	}

	// select Flexclient by default
	menu.Select(0)

	// Two-pane layout with fixed-width left nav so route expansion cannot
	// collapse/shrink the menu area.
	navWidthShim := canvas.NewRectangle(color.Transparent)
	navWidthShim.SetMinSize(fyne.NewSize(180, 1))
	navPanel := container.NewStack(navWidthShim, menu)
	mainLayout := container.NewBorder(nil, nil, navPanel, nil, contentStack)

	w.SetContent(mainLayout)
	log.Printf("GUI: content set (left menu + right pages), entering ShowAndRun")

	// Also run firewall refresh once after UI is live (so it's ready when you open About)
	go func() {
		time.Sleep(200 * time.Millisecond)
		fyne.Do(refreshFirewallStatus)
	}()

	w.ShowAndRun()
}
