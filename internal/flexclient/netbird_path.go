package flexclient

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// NetbirdCLIPath returns the resolved NetBird CLI path.
// Order:
// 1) NETBIRD_CLI env var (absolute or discoverable via PATH)
// 2) OS-specific common install locations
// 3) "netbird" via PATH lookup
// 4) fallback to literal "netbird"
func NetbirdCLIPath() string {
	if p := resolveCLIPath(strings.TrimSpace(os.Getenv("NETBIRD_CLI"))); p != "" {
		return p
	}

	for _, candidate := range defaultCLICandidates() {
		if p := resolveCLIPath(candidate); p != "" {
			return p
		}
	}

	return netbirdDefaultCLI
}

func netbirdCLIPath() string {
	return NetbirdCLIPath()
}

func defaultCLICandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/usr/local/bin/netbird",
			"/opt/homebrew/bin/netbird",
			"/Applications/NetBird.app/Contents/MacOS/netbird",
			netbirdDefaultCLI,
		}
	case "linux":
		return []string{
			"/usr/local/bin/netbird",
			"/usr/bin/netbird",
			"/snap/bin/netbird",
			netbirdDefaultCLI,
		}
	case "windows":
		return []string{
			`C:\Program Files\NetBird\netbird.exe`,
			`C:\Program Files (x86)\NetBird\netbird.exe`,
			"netbird.exe",
			netbirdDefaultCLI,
		}
	default:
		return []string{netbirdDefaultCLI}
	}
}

func resolveCLIPath(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}

	if filepath.IsAbs(candidate) {
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
		return ""
	}

	if resolved, err := exec.LookPath(candidate); err == nil && strings.TrimSpace(resolved) != "" {
		return resolved
	}

	return ""
}
