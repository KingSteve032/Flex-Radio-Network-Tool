package frnt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexclient"
)

const (
	AppName             = "Flex Radio Network Tool"
	Version             = "0.1.0" // bump as needed
	heartbeatListUpdate = 1 * time.Second
	discoveryActiveFor  = 10 * time.Second // RX "active" window

	// GitHub repo for updates (your repo)
	githubOwner = "KingSteve032"
	githubRepo  = "Flex-Radio-Network-Tool"
	// asset name to download from releases (must match uploaded Windows exe)
	updateAssetName = "flexclient-gui.exe"
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

// --- update logic ---

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func fetchLatestRelease() (*ghRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)
	log.Printf("Updates: checking %s", url)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", AppName+" "+Version)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GitHub API status %d: %s", resp.StatusCode, string(body))
	}

	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode error: %w", err)
	}
	log.Printf("Updates: latest tag %s", rel.TagName)
	return &rel, nil
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return v
}

func parseSemver(v string) (major, minor, patch int) {
	v = normalizeVersion(v)
	parts := strings.Split(v, ".")
	if len(parts) > 0 {
		fmt.Sscanf(parts[0], "%d", &major)
	}
	if len(parts) > 1 {
		fmt.Sscanf(parts[1], "%d", &minor)
	}
	if len(parts) > 2 {
		fmt.Sscanf(parts[2], "%d", &patch)
	}
	return
}

func isNewerVersion(current, latest string) bool {
	cMaj, cMin, cPatch := parseSemver(current)
	lMaj, lMin, lPatch := parseSemver(latest)

	if lMaj > cMaj {
		return true
	}
	if lMaj < cMaj {
		return false
	}
	if lMin > cMin {
		return true
	}
	if lMin < cMin {
		return false
	}
	return lPatch > cPatch
}

func findUpdateAsset(rel *ghRelease) (string, bool) {
	for _, a := range rel.Assets {
		if a.Name == updateAssetName {
			return a.BrowserDownloadURL, true
		}
	}
	return "", false
}

func downloadFile(url, path string) error {
	log.Printf("Updates: downloading from %s to %s", url, path)
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

func runWindowsUpdateBatch(currentExe, newExe string) error {
	dir := filepath.Dir(currentExe)
	exeName := filepath.Base(currentExe)
	batPath := filepath.Join(dir, "flexclient_update.bat")

	contents := fmt.Sprintf(`@echo off
echo Updating %s...
ping 127.0.0.1 -n 3 >nul
copy /y "%s" "%s" >nul
start "" "%s"
del "%s"
del "%%~f0"
`, AppName, newExe, currentExe, exeName, newExe)

	if err := os.WriteFile(batPath, []byte(contents), 0644); err != nil {
		return err
	}

	log.Printf("Updates: starting update batch %s", batPath)
	cmd := exec.Command("cmd", "/C", batPath)
	return cmd.Start()
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
	w.Resize(fyne.NewSize(650, 420))
	log.Printf("GUI: window created")

	// Route list
	routeList := widget.NewList(
		func() int {
			rs := flexclient.Routes()
			return len(rs)
		},
		func() fyne.CanvasObject {
			return widget.NewLabel("")
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			label := o.(*widget.Label)
			rs := flexclient.Routes()
			if i < 0 || i >= len(rs) {
				label.SetText("")
				return
			}
			r := rs[i]

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

			label.SetText(fmt.Sprintf("%s (%s) – %s – %s", r.ID, r.IP.String(), hbText, rxText))
		},
	)

	// Periodic refresh of routeList for heartbeat/discovery age
	go func() {
		log.Printf("GUI: starting routeList refresh ticker")
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

	// Version label & Update button
	versionLabel := widget.NewLabel(AppName + " Version: " + Version)
	updateBtn := widget.NewButton("Check for Updates", nil)

	var (
		clientMu      sync.Mutex
		clientCancel  context.CancelFunc
		clientRunning bool
	)

	startBtn.OnTapped = func() {
		log.Printf("GUI: Start clicked")
		clientMu.Lock()
		defer clientMu.Unlock()

		if clientRunning {
			log.Printf("GUI: client already running, ignoring Start")
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		clientCancel = cancel
		clientRunning = true

		startBtn.Disable()
		stopBtn.Enable()

		go flexclient.Start(ctx, Version)
	}

	stopBtn.OnTapped = func() {
		log.Printf("GUI: Stop clicked")
		clientMu.Lock()
		defer clientMu.Unlock()

		if !clientRunning {
			log.Printf("GUI: client not running, ignoring Stop")
			return
		}

		clientCancel()
		clientCancel = nil
		clientRunning = false

		startBtn.Enable()
		stopBtn.Disable()
	}

	updateBtn.OnTapped = func() {
		log.Printf("GUI: Check for Updates clicked")

		if runtime.GOOS != "windows" {
			dialog.ShowInformation("Updates", "Auto-update is only implemented for Windows binaries.", w)
			return
		}

		go func() {
			fyne.Do(func() {
				updateBtn.Disable()
				updateBtn.SetText("Checking…")
			})

			rel, err := fetchLatestRelease()
			if err != nil {
				log.Printf("Updates: check failed: %v", err)
				fyne.Do(func() {
					updateBtn.SetText("Check for Updates")
					updateBtn.Enable()
					dialog.ShowError(err, w)
				})
				return
			}

			latestTag := normalizeVersion(rel.TagName)
			currentTag := normalizeVersion(Version)

			if !isNewerVersion(currentTag, latestTag) {
				log.Printf("Updates: no update (current=%s latest=%s)", currentTag, latestTag)
				fyne.Do(func() {
					updateBtn.SetText("Check for Updates")
					updateBtn.Enable()
					dialog.ShowInformation("Updates",
						fmt.Sprintf("You are up to date.\nCurrent: %s\nLatest: %s", currentTag, latestTag), w)
				})
				return
			}

			log.Printf("Updates: new version available (current=%s latest=%s)", currentTag, latestTag)

			downloadURL, ok := findUpdateAsset(rel)
			if !ok {
				log.Printf("Updates: no asset named %s", updateAssetName)
				fyne.Do(func() {
					updateBtn.SetText("Check for Updates")
					updateBtn.Enable()
					dialog.ShowInformation("Updates",
						fmt.Sprintf("New version %s is available, but no %s asset was found.", latestTag, updateAssetName), w)
				})
				return
			}

			fyne.Do(func() {
				dialog.ShowConfirm(
					"Update Available",
					fmt.Sprintf("A new version is available.\nCurrent: %s\nLatest: %s\n\nDownload and install now?",
						currentTag, latestTag),
					func(ok bool) {
						if !ok {
							log.Printf("Updates: user declined update")
							updateBtn.SetText("Check for Updates")
							updateBtn.Enable()
							return
						}

						go func() {
							fyne.Do(func() {
								updateBtn.SetText("Downloading…")
								updateBtn.Disable()
							})

							exePath, err := os.Executable()
							if err != nil {
								log.Printf("Updates: os.Executable failed: %v", err)
								fyne.Do(func() {
									updateBtn.SetText("Check for Updates")
									updateBtn.Enable()
									dialog.ShowError(fmt.Errorf("cannot locate current executable: %w", err), w)
								})
								return
							}

							dir := filepath.Dir(exePath)
							newExe := filepath.Join(dir, "flexclient-gui.new.exe")

							if err := downloadFile(downloadURL, newExe); err != nil {
								log.Printf("Updates: download failed: %v", err)
								fyne.Do(func() {
									updateBtn.SetText("Check for Updates")
									updateBtn.Enable()
									dialog.ShowError(fmt.Errorf("download failed: %w", err), w)
								})
								return
							}

							if err := runWindowsUpdateBatch(exePath, newExe); err != nil {
								log.Printf("Updates: starting updater batch failed: %v", err)
								fyne.Do(func() {
									updateBtn.SetText("Check for Updates")
									updateBtn.Enable()
									dialog.ShowError(fmt.Errorf("failed to start updater: %w", err), w)
								})
								return
							}

							log.Printf("Updates: batch started, exiting")
							os.Exit(0)
						}()
					},
					w,
				)
			})
		}()
	}

	topBar := container.NewHBox(startBtn, stopBtn, widget.NewSeparator(), versionLabel, updateBtn)
	content := container.NewBorder(topBar, nil, nil, nil, routeList)
	w.SetContent(content)
	log.Printf("GUI: content set, entering ShowAndRun")

	w.ShowAndRun()
}
