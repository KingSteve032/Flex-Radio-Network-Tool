package frnt

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const sudoPasswordB64Token = "__FRNT_SUDO_PASSWORD_B64__"
const systemConfigPath = "/etc/frnt/flextool"

type adminTarget struct {
	User    string
	Host    string
	Port    int
	Display string
	// Optional per-target credentials. When blank, request-level defaults are used.
	SSHPassword  string
	SudoPassword string
}

type adminFlexToolConfig struct {
	Broadcast             bool
	ListenInterface       string
	SendInterface         string
	Debug                 bool
	NetBirdAPIToken       string
	NetBirdAPIURL         string
	DiscoveryDelaySeconds int
	SyncIntervalSeconds   int
	IgnoreRadios          string
	EnableVitaProxy       bool
	VitaProxyPort         int
	ProxyBasePort         int
	MultiProxy            bool
}

type adminBatchRequest struct {
	Targets        []adminTarget
	SSHPassword    string
	SudoPassword   string
	InstallCommand string
	ServiceName    string
	BinaryPath     string
	FlexToolConfig adminFlexToolConfig
	ApplyConfig    bool
	InstallService bool
	RestartService bool
	RunInstallCmd  bool
	ConnectTimeout time.Duration
	CommandTimeout time.Duration
}

func (r adminBatchRequest) sshPasswordFor(target adminTarget) string {
	if strings.TrimSpace(target.SSHPassword) != "" {
		return target.SSHPassword
	}
	return r.SSHPassword
}

func (r adminBatchRequest) sudoPasswordForTarget(target adminTarget) string {
	if strings.TrimSpace(target.SudoPassword) != "" {
		return target.SudoPassword
	}
	if strings.TrimSpace(r.SudoPassword) != "" {
		return r.SudoPassword
	}
	return r.sshPasswordFor(target)
}

func parseAdminTargets(raw, defaultUser string) ([]adminTarget, error) {
	defaultUser = strings.TrimSpace(defaultUser)
	if defaultUser == "" {
		defaultUser = "root"
	}

	var out []adminTarget
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Optional inline credential format:
		//   user@host[:port]
		//   user@host[:port] ssh_password
		//   user@host[:port] ssh_password sudo_password
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		endpoint := parts[0]
		targetSSHPassword := ""
		targetSudoPassword := ""
		if len(parts) >= 2 {
			targetSSHPassword = parts[1]
		}
		if len(parts) >= 3 {
			targetSudoPassword = parts[2]
		}

		userPart := defaultUser
		hostPart := endpoint
		if at := strings.LastIndex(endpoint, "@"); at > 0 {
			userPart = strings.TrimSpace(endpoint[:at])
			hostPart = strings.TrimSpace(endpoint[at+1:])
		}
		if userPart == "" || hostPart == "" {
			return nil, fmt.Errorf("line %d: expected [user@]host[:port]", i+1)
		}

		host, port, err := parseHostAndPort(hostPart)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}

		out = append(out, adminTarget{
			User:         userPart,
			Host:         host,
			Port:         port,
			Display:      fmt.Sprintf("%s@%s:%d", userPart, host, port),
			SSHPassword:  targetSSHPassword,
			SudoPassword: targetSudoPassword,
		})
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("no servers provided")
	}
	return out, nil
}

func parseHostAndPort(raw string) (string, int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, fmt.Errorf("empty host")
	}

	// bracketed IPv6 form: [addr]:port
	if strings.HasPrefix(raw, "[") {
		host, portStr, err := net.SplitHostPort(raw)
		if err != nil {
			return "", 0, fmt.Errorf("invalid target %q", raw)
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p <= 0 || p >= 65536 {
			return "", 0, fmt.Errorf("invalid port in %q", raw)
		}
		return host, p, nil
	}

	host := raw
	port := 22
	if idx := strings.LastIndex(raw, ":"); idx > 0 {
		maybePort := strings.TrimSpace(raw[idx+1:])
		if isDigits(maybePort) {
			p, err := strconv.Atoi(maybePort)
			if err != nil || p <= 0 || p >= 65536 {
				return "", 0, fmt.Errorf("invalid port in %q", raw)
			}
			host = strings.TrimSpace(raw[:idx])
			port = p
		}
	}

	if host == "" {
		return "", 0, fmt.Errorf("invalid host in %q", raw)
	}
	return host, port, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func renderFlexToolConfig(cfg adminFlexToolConfig) string {
	lines := []string{
		fmt.Sprintf("BROADCAST=%s", boolToLower(cfg.Broadcast)),
		fmt.Sprintf("LISTEN_INTERFACE=%s", strings.TrimSpace(cfg.ListenInterface)),
		fmt.Sprintf("SEND_INTERFACE=%s", strings.TrimSpace(cfg.SendInterface)),
		fmt.Sprintf("DEBUG=%s", boolToLower(cfg.Debug)),
		fmt.Sprintf("NETBIRD_API_TOKEN=%s", envQuote(cfg.NetBirdAPIToken)),
		fmt.Sprintf("NETBIRD_API_URL=%s", envQuote(cfg.NetBirdAPIURL)),
		fmt.Sprintf("DISCOVERY_DELAY_SECONDS=%d", cfg.DiscoveryDelaySeconds),
		fmt.Sprintf("SYNC_INTERVAL_SECONDS=%d", cfg.SyncIntervalSeconds),
		fmt.Sprintf("IGNORE_RADIOS=%s", strings.TrimSpace(cfg.IgnoreRadios)),
		fmt.Sprintf("ENABLE_VITA_PROXY=%s", boolToLower(cfg.EnableVitaProxy)),
		fmt.Sprintf("VITA_PROXY_PORT=%d", cfg.VitaProxyPort),
		fmt.Sprintf("PROXY_BASE_PORT=%d", cfg.ProxyBasePort),
		fmt.Sprintf("MULTI_PROXY=%s", boolToLower(cfg.MultiProxy)),
	}
	return strings.Join(lines, "\n") + "\n"
}

func boolToLower(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func envQuote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return `"` + s + `"`
}

func buildConfigApplyCommand(cfg adminFlexToolConfig, sudoPassword string) string {
	content := renderFlexToolConfig(cfg)
	tmp := "/tmp/frnt-flextool"

	lines := []string{
		"set -e",
		"cat > " + shQuote(tmp) + " <<'FLEXTOOL_EOF'",
		content + "FLEXTOOL_EOF",
		"chmod 600 " + shQuote(tmp),
		sudoWrap("mkdir -p "+shQuote(path.Dir(systemConfigPath)), sudoPassword),
		sudoWrap("install -m 0600 "+shQuote(tmp)+" "+shQuote(systemConfigPath), sudoPassword),
		"install -m 0600 " + shQuote(tmp) + " \"$HOME/.flextool\"",
		"rm -f " + shQuote(tmp),
	}
	return strings.Join(lines, "\n")
}

func buildInstallServiceCommand(serviceName, binaryPath string, sudoPassword string) string {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		serviceName = "frnt-listen.service"
	}
	binaryPath = strings.TrimSpace(binaryPath)
	if binaryPath == "" {
		binaryPath = "/usr/local/bin/frnt"
	}
	wd := path.Dir(binaryPath)
	if wd == "." || wd == "" {
		wd = "/usr/local/bin"
	}

	unit := fmt.Sprintf(`[Unit]
Description=Flex Radio Network Tool Server Listen
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
EnvironmentFile=-%s
ExecStart=%s --mode server --config %s listen
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
`, wd, systemConfigPath, binaryPath, systemConfigPath)

	tmp := "/tmp/" + serviceName
	var b strings.Builder
	b.WriteString("cat > ")
	b.WriteString(shQuote(tmp))
	b.WriteString(" <<'FRNT_UNIT_EOF'\n")
	b.WriteString(unit)
	b.WriteString("FRNT_UNIT_EOF\n")
	b.WriteString(sudoWrap("install -m 0644 "+shQuote(tmp)+" "+shQuote("/etc/systemd/system/"+serviceName), sudoPassword))
	b.WriteString("\n")
	b.WriteString(sudoWrap("systemctl daemon-reload", sudoPassword))
	b.WriteString("\n")
	b.WriteString(sudoWrap("systemctl enable "+shQuote(serviceName), sudoPassword))
	return b.String()
}

func buildRestartServiceCommand(serviceName, sudoPassword string) string {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		serviceName = "frnt-listen.service"
	}
	return strings.Join([]string{
		sudoWrap("systemctl reset-failed "+shQuote(serviceName)+" || true", sudoPassword),
		sudoWrap("systemctl restart "+shQuote(serviceName), sudoPassword),
		sudoWrap("systemctl is-active "+shQuote(serviceName), sudoPassword),
	}, "\n")
}

func buildAutoBootstrapCommand(repoURL, sourceDir, buildOutput, installPath, sudoPassword string) string {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		repoURL = "https://github.com/KingSteve032/Flex-Radio-Network-Tool.git"
	}
	sourceDir = strings.TrimSpace(sourceDir)
	if sourceDir == "" {
		sourceDir = "/opt/frnt/src"
	}
	buildOutput = strings.TrimSpace(buildOutput)
	if buildOutput == "" {
		buildOutput = "/opt/frnt/frnt"
	}
	installPath = strings.TrimSpace(installPath)
	if installPath == "" {
		installPath = "/usr/local/bin/frnt"
	}

	parentDir := path.Dir(sourceDir)
	buildDir := path.Dir(buildOutput)

	lines := []string{
		"set -e",
		"if command -v apt-get >/dev/null 2>&1; then",
		"  " + sudoWrap("apt-get update", sudoPassword),
		"  " + sudoWrap("DEBIAN_FRONTEND=noninteractive apt-get install -y git golang-go build-essential libpcap-dev", sudoPassword),
		"elif command -v dnf >/dev/null 2>&1; then",
		"  " + sudoWrap("dnf install -y git golang gcc gcc-c++ libpcap-devel", sudoPassword),
		"elif command -v yum >/dev/null 2>&1; then",
		"  " + sudoWrap("yum install -y git golang gcc gcc-c++ libpcap-devel", sudoPassword),
		"else",
		"  echo 'No supported package manager found. Expecting build deps to already exist.'",
		"fi",
		"mkdir -p " + shQuote(parentDir),
		"if [ ! -d " + shQuote(path.Join(sourceDir, ".git")) + " ]; then",
		"  git clone --depth 1 " + shQuote(repoURL) + " " + shQuote(sourceDir),
		"else",
		"  git -C " + shQuote(sourceDir) + " fetch --all --prune",
		"  git -C " + shQuote(sourceDir) + " pull --ff-only",
		"fi",
		"mkdir -p " + shQuote(buildDir),
		"cd " + shQuote(sourceDir),
		"go build -o " + shQuote(buildOutput) + " .",
		sudoWrap("install -m 0755 "+shQuote(buildOutput)+" "+shQuote(installPath), sudoPassword),
	}

	return strings.Join(lines, "\n")
}

func buildGitHubReleaseInstallCommand(repoFullName, installPath string) string {
	repoFullName = strings.TrimSpace(repoFullName)
	if repoFullName == "" {
		repoFullName = "KingSteve032/Flex-Radio-Network-Tool"
	}
	installPath = strings.TrimSpace(installPath)
	if installPath == "" {
		installPath = "/usr/local/bin/frnt"
	}

	lines := []string{
		"set -eu",
		"arch=\"$(uname -m)\"",
		"asset=\"\"",
		"case \"$arch\" in",
		"  x86_64|amd64) asset=\"frnt-linux-amd64\" ;;",
		"  aarch64|arm64) asset=\"frnt-linux-arm64\" ;;",
		"  armv7l|armv7|armhf) asset=\"frnt-linux-armv7\" ;;",
		"  *) echo \"Unsupported architecture: $arch\" >&2; exit 1 ;;",
		"esac",
		"url=\"https://github.com/" + repoFullName + "/releases/latest/download/${asset}\"",
		"install_path=" + shQuote(installPath),
		"sudo_password_b64=\"" + sudoPasswordB64Token + "\"",
		"tmp=\"$(mktemp /tmp/frnt-release.XXXXXX)\"",
		"trap 'rm -f \"$tmp\"' EXIT",
		"echo \"Downloading ${asset} from ${url}\"",
		"curl -fL --retry 3 --connect-timeout 10 -o \"$tmp\" \"$url\"",
		"if [ ! -s \"$tmp\" ]; then",
		"  echo \"Downloaded binary is missing or empty: ${tmp}\" >&2",
		"  exit 1",
		"fi",
		"chmod +x \"$tmp\"",
		"install_frnt_binary() {",
		"  src=\"$1\"",
		"  dst=\"$2\"",
		"  if [ -z \"$src\" ] || [ ! -f \"$src\" ]; then",
		"    echo \"Install source is missing: ${src}\" >&2",
		"    exit 1",
		"  fi",
		"  echo \"Installing ${src} to ${dst}\"",
		"  if sudo -n true >/dev/null 2>&1; then",
		"    sudo -n install -m 0755 \"$src\" \"$dst\"",
		"  elif [ -n \"$sudo_password_b64\" ]; then",
		"    sudo_password=\"$(printf '%s' \"$sudo_password_b64\" | base64 -d)\"",
		"    printf '%s\\n' \"$sudo_password\" | sudo -S -p '' install -m 0755 \"$src\" \"$dst\"",
		"  else",
		"    echo \"sudo password is required for install\" >&2",
		"    exit 1",
		"  fi",
		"}",
		"install_frnt_binary \"$tmp\" \"$install_path\"",
	}
	return strings.Join(lines, "\n")
}

func applyTargetSudoPasswordToken(command, sudoPassword string) string {
	if !strings.Contains(command, sudoPasswordB64Token) {
		return command
	}
	if strings.TrimSpace(sudoPassword) == "" {
		return strings.ReplaceAll(command, sudoPasswordB64Token, "")
	}
	enc := base64.StdEncoding.EncodeToString([]byte(sudoPassword))
	return strings.ReplaceAll(command, sudoPasswordB64Token, enc)
}

func buildServerInfoCommand(serviceName, binaryPath string) string {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		serviceName = "frnt-listen.service"
	}
	binaryPath = strings.TrimSpace(binaryPath)
	if binaryPath == "" {
		binaryPath = "/usr/local/bin/frnt"
	}
	binaryDir := path.Dir(binaryPath)
	configA := systemConfigPath
	configB := "$HOME/.flextool"
	configC := binaryDir + "/.flextool"

	lines := []string{
		"set -e",
		"echo \"HOST=$(hostname)\"",
		"echo \"KERNEL=$(uname -srmo)\"",
		"echo \"UPTIME=$(uptime -p || true)\"",
		"echo \"IP_ADDRS_BEGIN\"",
		"ip -4 -brief addr || true",
		"echo \"IP_ADDRS_END\"",
		"echo \"FRNT_VERSION_BEGIN\"",
		"if [ -x " + shQuote(binaryPath) + " ]; then " + shQuote(binaryPath) + " --version || true; else echo \"missing: " + binaryPath + "\"; fi",
		"echo \"FRNT_VERSION_END\"",
		"echo \"SERVICE_STATUS_BEGIN\"",
		"systemctl is-enabled " + shQuote(serviceName) + " || true",
		"systemctl is-active " + shQuote(serviceName) + " || true",
		"systemctl --no-pager -l status " + shQuote(serviceName) + " | sed -n '1,18p' || true",
		"echo \"SERVICE_STATUS_END\"",
		"echo \"CONFIG_PATHS_BEGIN\"",
		"for p in " + shQuote(configA) + " " + configB + " " + shQuote(configC) + "; do if [ -f \"$p\" ]; then echo \"$p\"; fi; done",
		"echo \"CONFIG_PATHS_END\"",
	}
	return strings.Join(lines, "\n")
}

func buildFetchConfigCommand(binaryPath string) string {
	binaryPath = strings.TrimSpace(binaryPath)
	if binaryPath == "" {
		binaryPath = "/usr/local/bin/frnt"
	}
	binaryDir := path.Dir(binaryPath)
	lines := []string{
		"set -e",
		"echo \"__FRNT_CONFIG_BEGIN__\"",
		"if [ -f " + shQuote(systemConfigPath) + " ]; then",
		"  cat " + shQuote(systemConfigPath),
		"elif [ -f \"$HOME/.flextool\" ]; then",
		"  cat \"$HOME/.flextool\"",
		"elif [ -f " + shQuote(binaryDir+"/.flextool") + " ]; then",
		"  cat " + shQuote(binaryDir+"/.flextool"),
		"else",
		"  echo \"# .flextool not found\"",
		"fi",
		"echo \"__FRNT_CONFIG_END__\"",
	}
	return strings.Join(lines, "\n")
}

func buildRebootCommand(sudoPassword string) string {
	return strings.Join([]string{
		"set -e",
		"echo \"Rebooting...\"",
		sudoWrap("systemctl reboot", sudoPassword),
	}, "\n")
}

func extractMarkedBlock(text, beginMarker, endMarker string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	start := strings.Index(text, beginMarker)
	if start < 0 {
		return ""
	}
	start += len(beginMarker)
	if start < len(text) && text[start] == '\n' {
		start++
	}
	rest := text[start:]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		return strings.TrimSpace(rest)
	}
	return strings.TrimSpace(rest[:end])
}

func parseFlexToolConfig(content string, base adminFlexToolConfig) adminFlexToolConfig {
	cfg := base
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		val = strings.Trim(val, `"`)
		switch key {
		case "BROADCAST":
			cfg.Broadcast = strings.EqualFold(val, "true")
		case "LISTEN_INTERFACE":
			cfg.ListenInterface = val
		case "SEND_INTERFACE":
			cfg.SendInterface = val
		case "DEBUG":
			cfg.Debug = strings.EqualFold(val, "true")
		case "NETBIRD_API_TOKEN":
			cfg.NetBirdAPIToken = val
		case "NETBIRD_API_URL":
			cfg.NetBirdAPIURL = val
		case "DISCOVERY_DELAY_SECONDS":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 {
				cfg.DiscoveryDelaySeconds = n
			}
		case "SYNC_INTERVAL_SECONDS":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 {
				cfg.SyncIntervalSeconds = n
			}
		case "IGNORE_RADIOS":
			cfg.IgnoreRadios = val
		case "ENABLE_VITA_PROXY":
			cfg.EnableVitaProxy = strings.EqualFold(val, "true")
		case "VITA_PROXY_PORT":
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				cfg.VitaProxyPort = n
			}
		case "PROXY_BASE_PORT":
			if n, err := strconv.Atoi(val); err == nil && n > 0 {
				cfg.ProxyBasePort = n
			}
		case "MULTI_PROXY":
			cfg.MultiProxy = strings.EqualFold(val, "true")
		}
	}
	return cfg
}

func sudoWrap(inner, sudoPassword string) string {
	if strings.TrimSpace(sudoPassword) == "" {
		return "sudo -n sh -lc " + shQuote(inner)
	}
	return "printf %s " + shQuote(sudoPassword+"\n") + " | sudo -S -p '' sh -lc " + shQuote(inner)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func runAdminBatch(req adminBatchRequest, logf func(string)) (okCount, failCount int) {
	if req.ConnectTimeout <= 0 {
		req.ConnectTimeout = 10 * time.Second
	}
	if req.CommandTimeout <= 0 {
		req.CommandTimeout = 15 * time.Minute
	}

	targets := make([]adminTarget, len(req.Targets))
	copy(targets, req.Targets)
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Display < targets[j].Display
	})

	for _, t := range targets {
		logf(fmt.Sprintf("[%s] starting batch actions", t.Display))
		sshPassword := req.sshPasswordFor(t)
		sudoPassword := req.sudoPasswordForTarget(t)

		if req.RunInstallCmd && strings.TrimSpace(req.InstallCommand) != "" {
			installCommand := applyTargetSudoPasswordToken(req.InstallCommand, sudoPassword)
			out, err := runSSHCommand(t, sshPassword, installCommand, req.ConnectTimeout, req.CommandTimeout)
			if out != "" {
				logf(fmt.Sprintf("[%s] install output:\n%s", t.Display, out))
			}
			if err != nil {
				logf(fmt.Sprintf("[%s] install command failed: %v", t.Display, err))
				failCount++
				continue
			}
			logf(fmt.Sprintf("[%s] install command complete", t.Display))
		}

		if req.ApplyConfig {
			cmd := buildConfigApplyCommand(req.FlexToolConfig, sudoPassword)
			out, err := runSSHCommand(t, sshPassword, cmd, req.ConnectTimeout, req.CommandTimeout)
			if out != "" {
				logf(fmt.Sprintf("[%s] config output:\n%s", t.Display, out))
			}
			if err != nil {
				logf(fmt.Sprintf("[%s] config apply failed: %v", t.Display, err))
				failCount++
				continue
			}
			logf(fmt.Sprintf("[%s] config applied", t.Display))
		}

		if req.InstallService {
			cmd := buildInstallServiceCommand(req.ServiceName, req.BinaryPath, sudoPassword)
			out, err := runSSHCommand(t, sshPassword, cmd, req.ConnectTimeout, req.CommandTimeout)
			if out != "" {
				logf(fmt.Sprintf("[%s] service install output:\n%s", t.Display, out))
			}
			if err != nil {
				logf(fmt.Sprintf("[%s] service install failed: %v", t.Display, err))
				failCount++
				continue
			}
			logf(fmt.Sprintf("[%s] service installed/enabled", t.Display))
		}

		if req.RestartService {
			cmd := buildRestartServiceCommand(req.ServiceName, sudoPassword)
			out, err := runSSHCommand(t, sshPassword, cmd, req.ConnectTimeout, req.CommandTimeout)
			if out != "" {
				logf(fmt.Sprintf("[%s] restart output:\n%s", t.Display, out))
			}
			if err != nil {
				logf(fmt.Sprintf("[%s] restart failed: %v", t.Display, err))
				failCount++
				continue
			}
			logf(fmt.Sprintf("[%s] restart complete", t.Display))
		}

		okCount++
	}

	return okCount, failCount
}

func (r adminBatchRequest) SudoPasswordOrDefault() string {
	if strings.TrimSpace(r.SudoPassword) != "" {
		return r.SudoPassword
	}
	return r.SSHPassword
}

func runAdminCommandOnTargets(req adminBatchRequest, command string, logf func(string)) (okCount, failCount int) {
	if req.ConnectTimeout <= 0 {
		req.ConnectTimeout = 10 * time.Second
	}
	if req.CommandTimeout <= 0 {
		req.CommandTimeout = 2 * time.Minute
	}

	targets := make([]adminTarget, len(req.Targets))
	copy(targets, req.Targets)
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Display < targets[j].Display
	})

	for _, t := range targets {
		logf(fmt.Sprintf("[%s] running command...", t.Display))
		out, err := runSSHCommand(t, req.sshPasswordFor(t), command, req.ConnectTimeout, req.CommandTimeout)
		if out != "" {
			logf(fmt.Sprintf("[%s] output:\n%s", t.Display, out))
		}
		if err != nil {
			logf(fmt.Sprintf("[%s] command failed: %v", t.Display, err))
			failCount++
			continue
		}
		okCount++
	}
	return okCount, failCount
}

func rebootTargetsAndWait(req adminBatchRequest, serviceName string, logf func(string)) (okCount, failCount int) {
	if req.ConnectTimeout <= 0 {
		req.ConnectTimeout = 10 * time.Second
	}
	if req.CommandTimeout <= 0 {
		req.CommandTimeout = 2 * time.Minute
	}
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		serviceName = "frnt-listen.service"
	}

	targets := make([]adminTarget, len(req.Targets))
	copy(targets, req.Targets)
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Display < targets[j].Display
	})

	for _, t := range targets {
		sshPassword := req.sshPasswordFor(t)
		sudoPassword := req.sudoPasswordForTarget(t)

		logf(fmt.Sprintf("[%s] sending reboot command...", t.Display))
		_, _ = runSSHCommand(t, sshPassword, buildRebootCommand(sudoPassword), req.ConnectTimeout, 20*time.Second)

		deadline := time.Now().Add(4 * time.Minute)
		seenOffline := false
		seenOnline := false
		lastErr := ""

		for time.Now().Before(deadline) {
			_, err := runSSHCommand(t, sshPassword, "echo up", 5*time.Second, 10*time.Second)
			if err != nil {
				seenOffline = true
				lastErr = err.Error()
				time.Sleep(5 * time.Second)
				continue
			}
			seenOnline = true
			if !seenOffline {
				time.Sleep(2 * time.Second)
				continue
			}
			break
		}

		if !seenOnline {
			logf(fmt.Sprintf("[%s] reboot wait failed: host did not come back online (%s)", t.Display, lastErr))
			failCount++
			continue
		}

		checkCmd := strings.Join([]string{
			"set -e",
			"systemctl is-active " + shQuote(serviceName) + " || true",
			"systemctl --no-pager -l status " + shQuote(serviceName) + " | sed -n '1,12p' || true",
		}, "\n")
		out, err := runSSHCommand(t, sshPassword, checkCmd, req.ConnectTimeout, 30*time.Second)
		if out != "" {
			logf(fmt.Sprintf("[%s] post-reboot service check:\n%s", t.Display, out))
		}
		if err != nil {
			logf(fmt.Sprintf("[%s] post-reboot service check failed: %v", t.Display, err))
			failCount++
			continue
		}
		logf(fmt.Sprintf("[%s] reboot complete and service check ran", t.Display))
		okCount++
	}
	return okCount, failCount
}

func runSSHCommand(target adminTarget, password, command string, connectTimeout, cmdTimeout time.Duration) (string, error) {
	auth := []ssh.AuthMethod{}
	if strings.TrimSpace(password) != "" {
		auth = append(auth, ssh.Password(password))
	}
	for _, signer := range loadDefaultSSHSigners() {
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if len(auth) == 0 {
		return "", fmt.Errorf("no SSH auth methods available (password empty and no local SSH keys found)")
	}

	cfg := &ssh.ClientConfig{
		User:            target.User,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // intentionally permissive for internal admin tool workflows
		Timeout:         connectTimeout,
	}

	addr := net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return "", fmt.Errorf("ssh dial %s failed: %w", addr, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("new ssh session failed: %w", err)
	}
	defer session.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- session.Run(command)
	}()

	select {
	case err := <-done:
		out := strings.TrimSpace(stdout.String())
		errOut := strings.TrimSpace(stderr.String())
		if errOut != "" {
			if out != "" {
				out += "\n"
			}
			out += errOut
		}
		if err != nil {
			return out, err
		}
		return out, nil
	case <-time.After(cmdTimeout):
		_ = session.Close()
		return strings.TrimSpace(stdout.String() + "\n" + stderr.String()), fmt.Errorf("command timed out after %s", cmdTimeout)
	}
}

func loadDefaultSSHSigners() []ssh.Signer {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return nil
	}

	candidates := []string{
		path.Join(home, ".ssh", "id_ed25519"),
		path.Join(home, ".ssh", "id_rsa"),
	}

	signers := make([]ssh.Signer, 0, len(candidates))
	for _, p := range candidates {
		key, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s, err := ssh.ParsePrivateKey(key)
		if err != nil {
			continue
		}
		signers = append(signers, s)
	}
	return signers
}
