package frnt

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/KingSteve032/Flex-Radio-Network-Tool/internal/flexclient"
)

const clientSettingsFileName = "flexclient-settings.json"

type proxySettingsFile struct {
	VPNMode       string            `json:"vpn_mode"`
	ManualRoutes  []manualRouteFile `json:"manual_routes"`
	ProxyBasePort int               `json:"proxy_base_port,omitempty"`
	RadioModes    map[string]string `json:"radio_modes"`
	IgnoredRoutes []string          `json:"ignored_routes"`
}

type manualRouteFile struct {
	ID string `json:"id"`
	IP string `json:"ip"`
}

func clientSettingsPath() string {
	exePath, err := os.Executable()
	if err != nil {
		return clientSettingsFileName
	}
	return filepath.Join(filepath.Dir(exePath), clientSettingsFileName)
}

func loadProxySettingsFromDisk() (proxySettingsFile, error) {
	path := clientSettingsPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return proxySettingsFile{}, err
	}

	var cfg proxySettingsFile
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return proxySettingsFile{}, err
	}
	if strings.TrimSpace(cfg.VPNMode) == "" {
		cfg.VPNMode = "netbird"
	}
	if cfg.RadioModes == nil {
		cfg.RadioModes = map[string]string{}
	}
	return cfg, nil
}

func saveProxySettingsToDisk(cfg proxySettingsFile) error {
	if cfg.RadioModes == nil {
		cfg.RadioModes = map[string]string{}
	}
	cfg.VPNMode = normalizeVPNModeText(cfg.VPNMode)
	cfg.ManualRoutes = normalizeManualRouteFiles(cfg.ManualRoutes)
	cfg.IgnoredRoutes = normalizeIgnoredRoutes(cfg.IgnoredRoutes)

	path := clientSettingsPath()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

func normalizeVPNModeText(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "manual" {
		return "manual"
	}
	return "netbird"
}

func normalizeManualRouteFiles(in []manualRouteFile) []manualRouteFile {
	if len(in) == 0 {
		return nil
	}
	out := make([]manualRouteFile, 0, len(in))
	seen := map[string]bool{}
	for _, route := range in {
		id := strings.TrimSpace(route.ID)
		ip := strings.TrimSpace(route.IP)
		if id == "" || ip == "" {
			continue
		}
		key := id + "=" + ip
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, manualRouteFile{ID: id, IP: ip})
	}
	return out
}

func manualRoutesFromSettings(in []manualRouteFile) []flexclient.ManualRoute {
	if len(in) == 0 {
		return nil
	}
	out := make([]flexclient.ManualRoute, 0, len(in))
	for _, route := range in {
		id := strings.TrimSpace(route.ID)
		ip := net.ParseIP(strings.TrimSpace(route.IP))
		if id == "" || ip == nil {
			continue
		}
		out = append(out, flexclient.ManualRoute{ID: id, IP: ip})
	}
	return out
}

func manualRoutesToSettings(in []flexclient.ManualRoute) []manualRouteFile {
	if len(in) == 0 {
		return nil
	}
	out := make([]manualRouteFile, 0, len(in))
	for _, route := range in {
		id := strings.TrimSpace(route.ID)
		if id == "" || route.IP == nil {
			continue
		}
		out = append(out, manualRouteFile{ID: id, IP: route.IP.String()})
	}
	return normalizeManualRouteFiles(out)
}

func normalizeIgnoredRoutes(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, route := range in {
		route = strings.TrimSpace(route)
		if route == "" {
			continue
		}
		set[route] = true
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for route := range set {
		out = append(out, route)
	}
	sort.Strings(out)
	return out
}

func parseRadioModesText(text string) (map[string]string, error) {
	out := map[string]string{}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("line %d: expected serial=mode", i+1)
		}

		serial := strings.ToLower(strings.TrimSpace(kv[0]))
		mode := normalizeRadioModeText(strings.TrimSpace(kv[1]))
		if serial == "" {
			return nil, fmt.Errorf("line %d: serial cannot be empty", i+1)
		}
		if mode == "" {
			return nil, fmt.Errorf("line %d: mode must be on or off", i+1)
		}
		out[serial] = mode
	}
	return out, nil
}

func normalizeRadioModeText(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "on", "direct", "proxy", "enabled", "enable", "true", "yes":
		return "direct"
	case "off", "disabled", "disable", "false", "no":
		return "off"
	default:
		return ""
	}
}

func formatRadioModesText(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		mode := "on"
		if normalizeRadioModeText(m[k]) == "off" {
			mode = "off"
		}
		lines = append(lines, fmt.Sprintf("%s=%s", k, mode))
	}
	return strings.Join(lines, "\n")
}
