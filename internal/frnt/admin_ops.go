package frnt

import (
	"bytes"
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

type adminTarget struct {
	User    string
	Host    string
	Port    int
	Display string
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

		userPart := defaultUser
		hostPart := line
		if at := strings.LastIndex(line, "@"); at > 0 {
			userPart = strings.TrimSpace(line[:at])
			hostPart = strings.TrimSpace(line[at+1:])
		}
		if userPart == "" || hostPart == "" {
			return nil, fmt.Errorf("line %d: expected [user@]host[:port]", i+1)
		}

		host, port, err := parseHostAndPort(hostPart)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}

		out = append(out, adminTarget{
			User:    userPart,
			Host:    host,
			Port:    port,
			Display: fmt.Sprintf("%s@%s:%d", userPart, host, port),
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

func buildConfigApplyCommand(cfg adminFlexToolConfig) string {
	content := renderFlexToolConfig(cfg)
	return "cat > \"$HOME/.flextool\" <<'FLEXTOOL_EOF'\n" + content + "FLEXTOOL_EOF\nchmod 600 \"$HOME/.flextool\""
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
ExecStart=%s --mode server listen
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
`, wd, binaryPath)

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
		if req.RunInstallCmd && strings.TrimSpace(req.InstallCommand) != "" {
			out, err := runSSHCommand(t, req.SSHPassword, req.InstallCommand, req.ConnectTimeout, req.CommandTimeout)
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
			cmd := buildConfigApplyCommand(req.FlexToolConfig)
			out, err := runSSHCommand(t, req.SSHPassword, cmd, req.ConnectTimeout, req.CommandTimeout)
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
			cmd := buildInstallServiceCommand(req.ServiceName, req.BinaryPath, req.SudoPasswordOrDefault())
			out, err := runSSHCommand(t, req.SSHPassword, cmd, req.ConnectTimeout, req.CommandTimeout)
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
			cmd := buildRestartServiceCommand(req.ServiceName, req.SudoPasswordOrDefault())
			out, err := runSSHCommand(t, req.SSHPassword, cmd, req.ConnectTimeout, req.CommandTimeout)
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
