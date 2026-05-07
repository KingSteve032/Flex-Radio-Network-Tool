//go:build windows || darwin

package frnt

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/buildinfo"
	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexclient"
	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/procutil"
)

const (
	AppName              = "Flex Radio Network Tool"
	heartbeatListUpdate  = 1 * time.Second
	discoveryActiveFor   = 10 * time.Second // RX "active" window
	netbirdStatusTimeout = 5 * time.Second
	updateCheckTimeout   = 8 * time.Second

	// fallback SmartSDR version (used only if winget detection fails)
	SmartSDRVersionFallback = "Unknown"
	releaseRepoFullName     = "KingSteve032/Flex-Radio-Network-Tool"
	smartSDRPaidMacURL      = "https://apps.apple.com/us/app/smartsdr-flexradio-systems/id1523656696?mt=12"
	aetherSDRRepoURL        = "https://github.com/ten9876/AetherSDR"
	w4carSmartSDRDefaultURL = "https://www.w4car.org/it-network"
)

var (
	w4carSmartSDRVersionRe = regexp.MustCompile(`SmartSDR Version:\s*([0-9]+(?:\.[0-9]+){1,3})`)
	version3Re             = regexp.MustCompile(`([0-9]+)\.([0-9]+)\.([0-9]+)`)
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
	log.Printf("===== %s v%s started =====", AppName, buildinfo.Short())
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

type latestReleaseResponse struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

func parseVersionTriplet(raw string) (major, minor, patch int, err error) {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	parts := strings.Split(v, ".")
	if len(parts) < 3 {
		return 0, 0, 0, fmt.Errorf("invalid version format: %q", raw)
	}

	parsePart := func(p string) (int, error) {
		p = strings.TrimSpace(p)
		if p == "" {
			return 0, fmt.Errorf("empty version part")
		}
		end := 0
		for end < len(p) && p[end] >= '0' && p[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0, fmt.Errorf("missing numeric prefix in %q", p)
		}
		return strconv.Atoi(p[:end])
	}

	major, err = parsePart(parts[0])
	if err != nil {
		return 0, 0, 0, err
	}
	minor, err = parsePart(parts[1])
	if err != nil {
		return 0, 0, 0, err
	}
	patch, err = parsePart(parts[2])
	if err != nil {
		return 0, 0, 0, err
	}
	return major, minor, patch, nil
}

func compareVersions(current, latest string) (int, error) {
	cMaj, cMin, cPat, err := parseVersionTriplet(current)
	if err != nil {
		return 0, err
	}
	lMaj, lMin, lPat, err := parseVersionTriplet(latest)
	if err != nil {
		return 0, err
	}

	switch {
	case cMaj < lMaj:
		return -1, nil
	case cMaj > lMaj:
		return 1, nil
	case cMin < lMin:
		return -1, nil
	case cMin > lMin:
		return 1, nil
	case cPat < lPat:
		return -1, nil
	case cPat > lPat:
		return 1, nil
	default:
		return 0, nil
	}
}

func fetchLatestRelease(ctx context.Context) (tagName, htmlURL string, err error) {
	endpoint := "https://api.github.com/repos/" + releaseRepoFullName + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "frnt/"+buildinfo.Short())

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("GitHub API status %d", resp.StatusCode)
	}

	var payload latestReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(payload.TagName) == "" {
		return "", "", fmt.Errorf("latest release missing tag_name")
	}
	if strings.TrimSpace(payload.HTMLURL) == "" {
		payload.HTMLURL = "https://github.com/" + releaseRepoFullName + "/releases/latest"
	}

	return strings.TrimSpace(payload.TagName), strings.TrimSpace(payload.HTMLURL), nil
}

func smartSDRWindowsURL() string {
	if v := strings.TrimSpace(os.Getenv("SMARTSDR_WINDOWS_URL")); v != "" {
		return v
	}
	return w4carSmartSDRDefaultURL
}

func normalizeVersion3(raw string) string {
	m := version3Re.FindStringSubmatch(strings.TrimSpace(raw))
	if len(m) != 4 {
		return ""
	}
	return fmt.Sprintf("%s.%s.%s", m[1], m[2], m[3])
}

func fetchW4CARSmartSDRTargetVersion(ctx context.Context) (string, error) {
	pageURL := smartSDRWindowsURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "frnt/"+buildinfo.Short())

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("W4CAR page returned status %d", resp.StatusCode)
	}

	var b strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		b.WriteString(scanner.Text())
		b.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	m := w4carSmartSDRVersionRe.FindStringSubmatch(b.String())
	if len(m) < 2 {
		return "", fmt.Errorf("could not find SmartSDR Version on W4CAR page")
	}
	return strings.TrimSpace(m[1]), nil
}

func installSmartSDRWindowsVersion(targetVersion string) (string, error) {
	targetVersion = strings.TrimSpace(targetVersion)
	if targetVersion == "" {
		return "", fmt.Errorf("target version is empty")
	}
	installVersion := targetVersion
	if norm := normalizeVersion3(targetVersion); norm != "" {
		installVersion = norm
	}
	downloadURL := fmt.Sprintf("https://smartsdr.flexradio.com/SmartSDR_v%s_x64.msi", installVersion)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "frnt/"+buildinfo.Short())
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download failed with status %d from %s", resp.StatusCode, downloadURL)
	}

	tmpFile, err := os.CreateTemp("", "smartsdr-*.msi")
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}()

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return "", err
	}
	if err := tmpFile.Close(); err != nil {
		return "", err
	}

	args := []string{"/i", tmpPath, "/passive", "/norestart"}
	cmd := exec.Command("msiexec", args...)
	procutil.HideWindow(cmd)
	out, err := cmd.CombinedOutput()
	combined := strings.TrimSpace(string(out))
	if combined == "" {
		combined = "(no installer output)"
	}
	combined = fmt.Sprintf("Downloaded: %s\nInstaller: msiexec %s\n%s", downloadURL, strings.Join(args, " "), combined)

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			// 3010/1641 are successful installs that require reboot.
			if code == 3010 || code == 1641 {
				return combined + fmt.Sprintf("\nInstaller exit code %d (reboot required).", code), nil
			}
		}
		return combined, fmt.Errorf("SmartSDR installer failed: %w", err)
	}
	return combined, nil
}

func loadAppIcon() fyne.Resource {
	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	cwd, _ := os.Getwd()

	candidates := []string{
		filepath.Join(exeDir, "icon.png"),
		filepath.Join(exeDir, "assets", "icon.png"),
		filepath.Join(cwd, "assets", "icon.png"),
	}

	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		res, err := fyne.LoadResourceFromPath(p)
		if err != nil {
			continue
		}
		log.Printf("GUI: loaded app icon from %s", p)
		return res
	}

	log.Printf("GUI: app icon not found (looked in icon.png and assets/icon.png)")
	return nil
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
	icon := loadAppIcon()
	if icon != nil {
		a.SetIcon(icon)
	}
	w := a.NewWindow(AppName)
	if icon != nil {
		w.SetIcon(icon)
	}
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
			go flexclient.Start(ctx, buildinfo.Short(), startupResult)

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
	appVersionLabel := widget.NewLabel(AppName + " Version: " + buildinfo.Full())

	netbirdVersionLabel := widget.NewLabel("NetBird: detecting...")
	smartSDRLabel := widget.NewLabel("SmartSDR Version: detecting...")
	smartInstallLabel := widget.NewLabel("SmartSDR Install: choose source below")
	updateLabel := widget.NewLabel("Update: not checked yet")

	latestReleaseURL := ""
	openReleaseBtn := widget.NewButton("Open Latest Release", func() {
		target := strings.TrimSpace(latestReleaseURL)
		if target == "" {
			target = "https://github.com/" + releaseRepoFullName + "/releases/latest"
		}
		u, err := url.Parse(target)
		if err != nil {
			dialog.ShowError(fmt.Errorf("invalid release URL: %w", err), w)
			return
		}
		if err := a.OpenURL(u); err != nil {
			dialog.ShowError(fmt.Errorf("unable to open release URL: %w", err), w)
		}
	})
	openReleaseBtn.Disable()
	checkUpdateBtn := widget.NewButton("Check For Updates", nil)

	openURL := func(rawURL, failPrefix string) {
		u, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil {
			dialog.ShowError(fmt.Errorf("%s: invalid URL: %w", failPrefix, err), w)
			return
		}
		if err := a.OpenURL(u); err != nil {
			dialog.ShowError(fmt.Errorf("%s: %w", failPrefix, err), w)
		}
	}

	installSmartSDRWindowsBtn := widget.NewButton("Windows SmartSDR (W4CAR)", nil)
	installSmartSDRWindowsBtn.OnTapped = func() {
		if runtime.GOOS != "windows" {
			dialog.ShowInformation("SmartSDR", "Windows SmartSDR install is only available on Windows.", w)
			return
		}

		installSmartSDRWindowsBtn.Disable()
		installSmartSDRWindowsBtn.SetText("Checking W4CAR...")
		smartInstallLabel.SetText("SmartSDR Install: checking W4CAR target version...")

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
			defer cancel()

			targetVersion, err := fetchW4CARSmartSDRTargetVersion(ctx)
			if err != nil {
				fyne.Do(func() {
					installSmartSDRWindowsBtn.SetText("Windows SmartSDR (W4CAR)")
					installSmartSDRWindowsBtn.Enable()
					smartInstallLabel.SetText("SmartSDR Install: failed to read W4CAR target version")
					dialog.ShowError(fmt.Errorf("W4CAR SmartSDR version check failed: %w", err), w)
				})
				return
			}

			currentVersion, curErr := getSmartSDRVersion()
			if curErr != nil {
				currentVersion = "not detected"
			}
			targetNorm := normalizeVersion3(targetVersion)
			currentNorm := normalizeVersion3(currentVersion)

			fyne.Do(func() {
				smartInstallLabel.SetText(fmt.Sprintf("SmartSDR Install: W4CAR target version is %s", targetVersion))
				if targetNorm != "" && currentNorm != "" && targetNorm == currentNorm {
					dialog.ShowInformation(
						"Install/Update SmartSDR",
						fmt.Sprintf("Installed SmartSDR (%s) already matches W4CAR target (%s).", currentNorm, targetNorm),
						w,
					)
					installSmartSDRWindowsBtn.SetText("Windows SmartSDR (W4CAR)")
					installSmartSDRWindowsBtn.Enable()
					return
				}
				dialog.ShowConfirm(
					"Install/Update SmartSDR",
					fmt.Sprintf("W4CAR target version: %s\nInstalled: %s\n\nUsing comparison: %s vs %s\n\nInstall/update now from smartsdr.flexradio.com?",
						targetVersion, currentVersion,
						func() string {
							if targetNorm == "" {
								return "n/a"
							}
							return targetNorm
						}(),
						func() string {
							if currentNorm == "" {
								return "n/a"
							}
							return currentNorm
						}(),
					),
					func(doInstall bool) {
						if !doInstall {
							installSmartSDRWindowsBtn.SetText("Windows SmartSDR (W4CAR)")
							installSmartSDRWindowsBtn.Enable()
							return
						}

						installSmartSDRWindowsBtn.Disable()
						installSmartSDRWindowsBtn.SetText("Installing...")
						go func() {
							output, installErr := installSmartSDRWindowsVersion(targetVersion)
							fyne.Do(func() {
								installSmartSDRWindowsBtn.SetText("Windows SmartSDR (W4CAR)")
								installSmartSDRWindowsBtn.Enable()
								if installErr != nil {
									log.Printf("SmartSDR install failed: %v\n%s", installErr, output)
									dialog.ShowInformation(
										"SmartSDR Install",
										"Automatic install failed. Opening W4CAR page for manual install.",
										w,
									)
									openURL(smartSDRWindowsURL(), "Unable to open W4CAR SmartSDR page")
									return
								}
								log.Printf("SmartSDR install output:\n%s", output)
								dialog.ShowInformation("SmartSDR Install", "SmartSDR install/update command completed.", w)
							})
						}()
					},
					w,
				)
			})
		}()
	}
	installSmartSDRPaidBtn := widget.NewButton("Paid SmartSDR (App Store)", func() {
		openURL(smartSDRPaidMacURL, "Unable to open SmartSDR App Store page")
	})
	installSmartSDRFreeBtn := widget.NewButton("Free AetherSDR (GitHub)", func() {
		openURL(aetherSDRRepoURL, "Unable to open AetherSDR page")
	})
	smartInstallDetail := widget.NewLabel("")
	switch runtime.GOOS {
	case "windows":
		smartInstallDetail.SetText("Windows: button checks W4CAR SmartSDR target version, then installs/updates from smartsdr.flexradio.com.")
	case "darwin":
		smartInstallDetail.SetText("macOS: choose Paid SmartSDR in App Store or Free AetherSDR.")
	default:
		smartInstallDetail.SetText("Linux: use Free AetherSDR. (Paid SmartSDR App Store page is macOS-only.)")
	}

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

	runUpdateCheck := func(manual bool) {
		currentVersion := buildinfo.Short()
		updateLabel.SetText("Update: checking...")
		checkUpdateBtn.Disable()

		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
			defer cancel()

			latestTag, releaseURL, err := fetchLatestRelease(ctx)
			fyne.Do(func() {
				defer checkUpdateBtn.Enable()

				if err != nil {
					log.Printf("About: update check failed: %v", err)
					updateLabel.SetText("Update: check failed")
					if manual {
						dialog.ShowInformation("Update Check", "Unable to check for updates right now.", w)
					}
					return
				}

				latestReleaseURL = releaseURL
				openReleaseBtn.Enable()

				cmp, cmpErr := compareVersions(currentVersion, latestTag)
				if cmpErr != nil {
					log.Printf("About: version compare failed current=%s latest=%s err=%v", currentVersion, latestTag, cmpErr)
					updateLabel.SetText("Update: latest " + latestTag + " (comparison unavailable)")
					if manual {
						dialog.ShowInformation("Update Check", fmt.Sprintf("Latest release is %s.", latestTag), w)
					}
					return
				}

				if cmp < 0 {
					updateLabel.SetText(fmt.Sprintf("Update: %s available (current %s)", latestTag, currentVersion))
					dialog.ShowConfirm(
						"Update Available",
						fmt.Sprintf("A new version (%s) is available.\nOpen releases page now?", latestTag),
						func(open bool) {
							if !open {
								return
							}
							u, err := url.Parse(releaseURL)
							if err != nil {
								dialog.ShowError(fmt.Errorf("invalid release URL: %w", err), w)
								return
							}
							if err := a.OpenURL(u); err != nil {
								dialog.ShowError(fmt.Errorf("unable to open release URL: %w", err), w)
							}
						},
						w,
					)
					return
				}

				updateLabel.SetText(fmt.Sprintf("Update: up to date (%s)", currentVersion))
				if manual {
					dialog.ShowInformation("Update Check", "You are on the latest version.", w)
				}
			})
		}()
	}
	checkUpdateBtn.OnTapped = func() {
		runUpdateCheck(true)
	}

	aboutPage := container.NewVBox(
		aboutHeader,
		appVersionLabel,
		netbirdVersionLabel,
		smartSDRLabel,
		smartInstallLabel,
		container.NewHBox(installSmartSDRWindowsBtn, installSmartSDRPaidBtn, installSmartSDRFreeBtn),
		smartInstallDetail,
		updateLabel,
		container.NewHBox(checkUpdateBtn, openReleaseBtn),
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
	adminHint := widget.NewLabel("Install from GitHub release, pull/edit/repush .flextool, restart service, and reboot hosts with service checks.")
	adminStatus := widget.NewLabel("Idle.")
	adminStatus.Wrapping = fyne.TextWrapWord

	adminTargetsEntry := widget.NewMultiLineEntry()
	adminTargetsEntry.SetPlaceHolder("one per line: user@host[:port] [ssh_password] [sudo_password]\nw4car@10.2.0.4 Chesapeake1!\nroot@10.10.1.50:22 sshpass sudopass")
	adminTargetsEntry.SetMinRowsVisible(6)

	adminDefaultUser := widget.NewEntry()
	adminDefaultUser.SetText("root")

	adminSSHPassword := widget.NewEntry()
	adminSSHPassword.SetPlaceHolder("default SSH password (optional)")

	adminSudoPassword := widget.NewEntry()
	adminSudoPassword.SetPlaceHolder("default sudo password (blank = SSH password)")

	adminQuickUser := widget.NewEntry()
	adminQuickUser.SetPlaceHolder("user")
	adminQuickUser.SetText("w4car")

	adminQuickHost := widget.NewEntry()
	adminQuickHost.SetPlaceHolder("host or IP")

	adminQuickPort := widget.NewEntry()
	adminQuickPort.SetPlaceHolder("22")
	adminQuickPort.SetText("22")

	adminQuickSSH := widget.NewEntry()
	adminQuickSSH.SetPlaceHolder("ssh password")

	adminQuickSudo := widget.NewEntry()
	adminQuickSudo.SetPlaceHolder("sudo password")

	adminServiceName := widget.NewEntry()
	adminServiceName.SetText("frnt-listen.service")

	adminBinaryPath := widget.NewEntry()
	adminBinaryPath.SetText("/usr/local/bin/frnt")

	adminReleaseRepo := widget.NewEntry()
	adminReleaseRepo.SetText("KingSteve032/Flex-Radio-Network-Tool")

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
	adminLog.SetMinRowsVisible(22)
	adminLog.SetPlaceHolder("Admin log output will appear here...")

	appendAdminLog := func(line string) {
		fyne.Do(func() {
			ts := time.Now().Format("15:04:05")
			line = strings.TrimSpace(line)
			if line == "" {
				return
			}
			prefixed := "[" + ts + "] " + line
			if strings.TrimSpace(adminLog.Text) == "" {
				adminLog.SetText(prefixed)
			} else {
				adminLog.SetText(adminLog.Text + "\n" + prefixed)
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

	loadConfigFromForm := func() (adminFlexToolConfig, error) {
		discoveryDelay, err := parsePositiveIntField("Discovery Delay", adminDiscoveryDelay.Text)
		if err != nil {
			return adminFlexToolConfig{}, err
		}
		syncInterval, err := parsePositiveIntField("Sync Interval", adminSyncInterval.Text)
		if err != nil {
			return adminFlexToolConfig{}, err
		}
		vitaPort, err := parsePositiveIntField("VITA Proxy Port", adminVitaPort.Text)
		if err != nil {
			return adminFlexToolConfig{}, err
		}
		proxyBase, err := parsePositiveIntField("Proxy Base Port", adminProxyBasePort.Text)
		if err != nil {
			return adminFlexToolConfig{}, err
		}
		return adminFlexToolConfig{
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
		}, nil
	}

	applyConfigToForm := func(cfg adminFlexToolConfig) {
		adminBroadcast.SetChecked(cfg.Broadcast)
		adminListenIF.SetText(cfg.ListenInterface)
		adminSendIF.SetText(cfg.SendInterface)
		adminDebug.SetChecked(cfg.Debug)
		adminAPIToken.SetText(cfg.NetBirdAPIToken)
		adminAPIURL.SetText(cfg.NetBirdAPIURL)
		adminDiscoveryDelay.SetText(strconv.Itoa(cfg.DiscoveryDelaySeconds))
		adminSyncInterval.SetText(strconv.Itoa(cfg.SyncIntervalSeconds))
		adminIgnoreRadios.SetText(cfg.IgnoreRadios)
		adminEnableVita.SetChecked(cfg.EnableVitaProxy)
		adminVitaPort.SetText(strconv.Itoa(cfg.VitaProxyPort))
		adminProxyBasePort.SetText(strconv.Itoa(cfg.ProxyBasePort))
		adminMultiProxy.SetChecked(cfg.MultiProxy)
	}

	buildAdminRequest := func(applyConfig, runInstall, installService, restartService bool, installCommandOverride string) (adminBatchRequest, error) {
		targets, err := parseAdminTargets(adminTargetsEntry.Text, adminDefaultUser.Text)
		if err != nil {
			return adminBatchRequest{}, err
		}
		cfg, err := loadConfigFromForm()
		if err != nil {
			return adminBatchRequest{}, err
		}

		installCommand := ""
		if runInstall {
			installCommand = strings.TrimSpace(installCommandOverride)
		}

		return adminBatchRequest{
			Targets:        targets,
			SSHPassword:    strings.TrimSpace(adminSSHPassword.Text),
			SudoPassword:   strings.TrimSpace(adminSudoPassword.Text),
			InstallCommand: installCommand,
			ServiceName:    strings.TrimSpace(adminServiceName.Text),
			BinaryPath:     strings.TrimSpace(adminBinaryPath.Text),
			FlexToolConfig: cfg,
			ApplyConfig:    applyConfig,
			InstallService: installService,
			RestartService: restartService,
			RunInstallCmd:  runInstall && strings.TrimSpace(installCommand) != "",
		}, nil
	}

	runAdminBatchAction := func(title string, applyConfig, runInstall, installService, restartService bool, installCommandOverride string) {
		req, err := buildAdminRequest(applyConfig, runInstall, installService, restartService, installCommandOverride)
		if err != nil {
			dialog.ShowError(err, w)
			adminStatus.SetText("Input error: " + err.Error())
			return
		}

		adminStatus.SetText(title + " running...")
		appendAdminLog(title + " started")
		go func() {
			okCount, failCount := runAdminBatch(req, appendAdminLog)
			fyne.Do(func() {
				adminStatus.SetText(fmt.Sprintf("%s done. Success=%d Failed=%d", title, okCount, failCount))
			})
		}()
	}

	runAdminCommandAction := func(title, cmd string) {
		req, err := buildAdminRequest(false, false, false, false, "")
		if err != nil {
			dialog.ShowError(err, w)
			adminStatus.SetText("Input error: " + err.Error())
			return
		}
		adminStatus.SetText(title + " running...")
		appendAdminLog(title + " started")
		go func() {
			okCount, failCount := runAdminCommandOnTargets(req, cmd, appendAdminLog)
			fyne.Do(func() {
				adminStatus.SetText(fmt.Sprintf("%s done. Success=%d Failed=%d", title, okCount, failCount))
			})
		}()
	}

	adminAddTargetBtn := widget.NewButton("Add Target", func() {
		user := strings.TrimSpace(adminQuickUser.Text)
		host := strings.TrimSpace(adminQuickHost.Text)
		if user == "" {
			user = strings.TrimSpace(adminDefaultUser.Text)
		}
		if user == "" || host == "" {
			dialog.ShowError(fmt.Errorf("quick add requires user and host"), w)
			return
		}
		port := strings.TrimSpace(adminQuickPort.Text)
		line := user + "@" + host
		if port != "" && port != "22" {
			line += ":" + port
		}
		sshPass := strings.TrimSpace(adminQuickSSH.Text)
		sudoPass := strings.TrimSpace(adminQuickSudo.Text)
		if sshPass != "" {
			line += " " + sshPass
		}
		if sudoPass != "" {
			if sshPass == "" {
				line += " "
			}
			line += " " + sudoPass
		}
		if strings.TrimSpace(adminTargetsEntry.Text) == "" {
			adminTargetsEntry.SetText(line)
		} else {
			adminTargetsEntry.SetText(strings.TrimRight(adminTargetsEntry.Text, "\n") + "\n" + line)
		}
		appendAdminLog("Added target " + user + "@" + host)
	})

	adminClearTargetsBtn := widget.NewButton("Clear Targets", func() {
		adminTargetsEntry.SetText("")
	})

	adminInstallReleaseBtn := widget.NewButton("Install From GitHub", func() {
		repo := strings.TrimSpace(adminReleaseRepo.Text)
		cmd := buildGitHubReleaseInstallCommand(repo, strings.TrimSpace(adminBinaryPath.Text))
		runAdminBatchAction("Install From GitHub", false, true, false, false, cmd)
	})

	adminPullInfoBtn := widget.NewButton("Pull Server Info", func() {
		cmd := buildServerInfoCommand(strings.TrimSpace(adminServiceName.Text), strings.TrimSpace(adminBinaryPath.Text))
		runAdminCommandAction("Pull Server Info", cmd)
	})

	adminPullConfigBtn := widget.NewButton("Pull Config", func() {
		req, err := buildAdminRequest(false, false, false, false, "")
		if err != nil {
			dialog.ShowError(err, w)
			adminStatus.SetText("Input error: " + err.Error())
			return
		}
		if len(req.Targets) == 0 {
			dialog.ShowError(fmt.Errorf("no targets provided"), w)
			return
		}
		target := req.Targets[0]
		cmd := buildFetchConfigCommand(strings.TrimSpace(adminBinaryPath.Text))
		adminStatus.SetText("Pulling config from " + target.Display + "...")
		appendAdminLog("Pulling config from " + target.Display)
		go func() {
			out, err := runSSHCommand(target, req.sshPasswordFor(target), cmd, 10*time.Second, 45*time.Second)
			if out != "" {
				appendAdminLog(fmt.Sprintf("[%s] config output:\n%s", target.Display, out))
			}
			if err != nil {
				fyne.Do(func() {
					adminStatus.SetText("Pull config failed: " + err.Error())
					dialog.ShowError(err, w)
				})
				return
			}

			block := extractMarkedBlock(out, "__FRNT_CONFIG_BEGIN__", "__FRNT_CONFIG_END__")
			if strings.TrimSpace(block) == "" {
				fyne.Do(func() {
					adminStatus.SetText("Pull config failed: no block returned")
					dialog.ShowError(fmt.Errorf("remote config block not found"), w)
				})
				return
			}

			current, _ := loadConfigFromForm()
			parsed := parseFlexToolConfig(block, current)
			fyne.Do(func() {
				applyConfigToForm(parsed)
				adminStatus.SetText("Pulled config from " + target.Display)
			})
		}()
	})

	adminPushConfigBtn := widget.NewButton("Push Config", func() {
		runAdminBatchAction("Push Config", true, false, false, false, "")
	})

	adminRestartServiceBtn := widget.NewButton("Restart Service", func() {
		runAdminBatchAction("Restart Service", false, false, false, true, "")
	})

	adminDeployBtn := widget.NewButton("Install + Push Config + Restart", func() {
		repo := strings.TrimSpace(adminReleaseRepo.Text)
		cmd := buildGitHubReleaseInstallCommand(repo, strings.TrimSpace(adminBinaryPath.Text))
		runAdminBatchAction("Deploy", true, true, true, true, cmd)
	})

	adminRebootBtn := widget.NewButton("Reboot + Verify Service", func() {
		req, err := buildAdminRequest(false, false, false, false, "")
		if err != nil {
			dialog.ShowError(err, w)
			adminStatus.SetText("Input error: " + err.Error())
			return
		}
		adminStatus.SetText("Reboot workflow running...")
		appendAdminLog("Reboot workflow started")
		go func() {
			okCount, failCount := rebootTargetsAndWait(req, strings.TrimSpace(adminServiceName.Text), appendAdminLog)
			fyne.Do(func() {
				adminStatus.SetText(fmt.Sprintf("Reboot workflow done. Success=%d Failed=%d", okCount, failCount))
			})
		}()
	})

	adminClearLogBtn := widget.NewButton("Clear Log", func() {
		adminLog.SetText("")
	})

	adminTargetsForm := widget.NewForm(
		widget.NewFormItem("Default SSH User", adminDefaultUser),
		widget.NewFormItem("Default SSH Password", adminSSHPassword),
		widget.NewFormItem("Default Sudo Password", adminSudoPassword),
		widget.NewFormItem("Targets", adminTargetsEntry),
	)

	adminQuickAddRow := container.NewVBox(
		widget.NewLabelWithStyle("Quick Add Target", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewGridWithColumns(3, adminQuickUser, adminQuickHost, adminQuickPort),
		container.NewGridWithColumns(2, adminQuickSSH, adminQuickSudo),
		container.NewHBox(adminAddTargetBtn, adminClearTargetsBtn),
	)

	adminConfigForm := widget.NewForm(
		widget.NewFormItem("Release Repo (owner/name)", adminReleaseRepo),
		widget.NewFormItem("Service Name", adminServiceName),
		widget.NewFormItem("Binary Path", adminBinaryPath),
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

	adminActions := container.NewVBox(
		widget.NewLabelWithStyle("Actions", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewHBox(adminInstallReleaseBtn, adminDeployBtn),
		container.NewHBox(adminPullInfoBtn, adminPullConfigBtn),
		container.NewHBox(adminPushConfigBtn, adminRestartServiceBtn),
		container.NewHBox(adminRebootBtn),
	)

	targetsCard := widget.NewCard("Targets", "Use one line per server", container.NewVBox(adminTargetsForm, adminQuickAddRow))
	configCard := widget.NewCard(".flextool Config", "Pull from server, edit, then repush", adminConfigForm)
	actionsCard := widget.NewCard("Operations", "All actions write to the log", adminActions)

	leftAdminPanel := container.NewVBox(targetsCard, configCard, actionsCard)
	logPanel := widget.NewCard("Execution Log", "", container.NewBorder(container.NewHBox(layout.NewSpacer(), adminClearLogBtn), nil, nil, nil, adminLog))

	adminSplit := container.NewHSplit(container.NewVScroll(leftAdminPanel), logPanel)
	adminSplit.Offset = 0.64

	adminPage := container.NewBorder(
		container.NewVBox(
			adminHeader,
			adminHint,
			adminStatus,
			widget.NewSeparator(),
		),
		nil,
		nil,
		nil,
		adminSplit,
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

	// Startup update check (non-blocking) shortly after launch.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		fyne.Do(func() {
			runUpdateCheck(false)
		})
	}()

	w.ShowAndRun()
}
