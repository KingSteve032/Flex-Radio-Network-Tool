package frnt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const clientSettingsFileName = "flexclient-settings.json"

type proxySettingsFile struct {
	ProxyBasePort int               `json:"proxy_base_port"`
	RadioModes    map[string]string `json:"radio_modes"`
	IgnoredRoutes []string          `json:"ignored_routes"`
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
	if cfg.ProxyBasePort == 0 {
		cfg.ProxyBasePort = 30000
	}
	if cfg.RadioModes == nil {
		cfg.RadioModes = map[string]string{}
	}
	return cfg, nil
}

func saveProxySettingsToDisk(cfg proxySettingsFile) error {
	if cfg.ProxyBasePort < 1024 || cfg.ProxyBasePort > 65535 {
		return fmt.Errorf("proxy base port must be between 1024 and 65535")
	}
	if cfg.RadioModes == nil {
		cfg.RadioModes = map[string]string{}
	}
	cfg.IgnoredRoutes = normalizeIgnoredRoutes(cfg.IgnoredRoutes)

	path := clientSettingsPath()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
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

func parseProxyBasePort(s string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid proxy base port")
	}
	if p < 1024 || p > 65535 {
		return 0, fmt.Errorf("proxy base port must be between 1024 and 65535")
	}
	return p, nil
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
		mode := strings.ToLower(strings.TrimSpace(kv[1]))
		if serial == "" {
			return nil, fmt.Errorf("line %d: serial cannot be empty", i+1)
		}
		if mode != "direct" && mode != "proxy" && mode != "off" {
			return nil, fmt.Errorf("line %d: mode must be direct, proxy, or off", i+1)
		}
		out[serial] = mode
	}
	return out, nil
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
		lines = append(lines, fmt.Sprintf("%s=%s", k, m[k]))
	}
	return strings.Join(lines, "\n")
}
