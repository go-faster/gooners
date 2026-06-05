package session

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gosshconfig "github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Config struct {
	Machine    string
	User       string
	Port       int
	KeyPath    string
	Password   string
	TimeoutSec int
	KnownHosts string
	// ProxyJump overrides the ProxyJump directive from ~/.ssh/config.
	// Set to "none" to disable jumping even if ssh_config specifies one.
	ProxyJump string
}

// clientConfig returns the ssh.ClientConfig plus two addresses:
//   - tcpAddr: resolved address for the TCP dial (honors HostName)
//   - sshAddr: alias:port for the SSH handshake, which is what known_hosts is
//     keyed on — OpenSSH stores/checks keys under the name the user typed, not
//     the resolved IP.
func (c Config) clientConfig(cfg *gosshconfig.UserSettings) (cc *ssh.ClientConfig, tcpAddr, sshAddr string, err error) {
	if cfg == nil {
		cfg = &gosshconfig.UserSettings{IgnoreErrors: false}
	}
	usr, alias, port := parseTarget(c.Machine)
	if alias == "" {
		alias = c.Machine
	}

	// alias is the original hostname before HostName resolution — used for all
	// ssh_config lookups, matching OpenSSH semantics.
	host := alias
	if cfgHostname := cfg.Get(alias, "HostName"); cfgHostname != "" && cfgHostname != alias {
		host = cfgHostname
	}
	if usr == "" {
		if cfgUser := cfg.Get(alias, "User"); cfgUser != "" {
			usr = cfgUser
		}
	}
	if port == 0 {
		if cfgPort := cfg.Get(alias, "Port"); cfgPort != "" {
			if p, err := strconv.Atoi(cfgPort); err == nil {
				port = p
			}
		}
	}

	// Explicit Config fields override ssh_config.
	if c.User != "" {
		usr = c.User
	}
	if c.Port != 0 {
		port = c.Port
	}
	if usr == "" {
		usr = currentUser()
	}
	if port == 0 {
		port = 22
	}
	portStr := strconv.Itoa(port)
	tcpAddr = net.JoinHostPort(host, portStr)
	sshAddr = net.JoinHostPort(alias, portStr)

	timeout := 30 * time.Second
	if c.TimeoutSec > 0 {
		if c.TimeoutSec > 3600 {
			c.TimeoutSec = 3600
		}
		timeout = time.Duration(c.TimeoutSec) * time.Second
	}

	// When HostName resolution changes the address, check both the alias (how
	// known_hosts is typically keyed by the user) and the resolved IP (how
	// ssh-keyscan or direct-IP connections store entries).
	var hkcb ssh.HostKeyCallback
	if host != alias {
		hkcb = multiAddrHostKeyCallback(c.KnownHosts, sshAddr, tcpAddr)
	} else {
		hkcb = hostKeyCallback(c.KnownHosts)
	}

	auth, err := authMethods(cfg, c, alias, usr)
	if err != nil {
		return nil, "", "", err
	}

	return &ssh.ClientConfig{
		User:            usr,
		Auth:            auth,
		HostKeyCallback: hkcb,
		Timeout:         timeout,
	}, tcpAddr, sshAddr, nil
}

// dial opens an SSH connection to c.Machine, following ProxyJump / ProxyCommand
// directives from ~/.ssh/config (unless overridden by Config fields).
func (c Config) dial() (client *ssh.Client, userAgent, banner string, err error) {
	cfg := &gosshconfig.UserSettings{IgnoreErrors: false}
	cc, tcpAddr, sshAddr, err := c.clientConfig(cfg)
	if err != nil {
		return nil, "", "", err
	}

	var targetBanner string
	cc.BannerCallback = func(message string) error {
		targetBanner = message
		return nil
	}

	_, alias, _ := parseTarget(c.Machine)
	if alias == "" {
		alias = c.Machine
	}

	slog.Debug("ssh dial start", "machine", c.Machine, "addr", tcpAddr, "timeout", cc.Timeout)

	jump := c.ProxyJump
	if jump == "" {
		jump = strings.TrimSpace(cfg.Get(alias, "ProxyJump"))
	}

	switch {
	case jump != "" && jump != "none":
		client, err = tunnelThrough(cfg, tcpAddr, sshAddr, cc, jump, c.KnownHosts)
	default:
		proxyCmd := cfg.Get(alias, "ProxyCommand")
		if proxyCmd != "" && proxyCmd != "none" {
			var conn net.Conn
			conn, err = dialProxyCommand(proxyCmd, tcpAddr, cc.User)
			if err != nil {
				slog.Debug("ssh proxycommand dial failed", "machine", c.Machine, "err", err)
				return nil, "", "", err
			}
			ncc, chans, reqs, cerr := ssh.NewClientConn(conn, sshAddr, cc)
			if cerr != nil {
				_ = conn.Close()
				slog.Debug("ssh NewClientConn (proxycommand) failed", "machine", c.Machine, "err", cerr)
				return nil, "", "", cerr
			}
			client = ssh.NewClient(ncc, chans, reqs)
		} else {
			// Use net.Dial + NewClientConn so we can pass sshAddr (alias:port)
			// to the SSH handshake. known_hosts entries are stored under the
			// alias name, not the resolved IP, matching OpenSSH behavior.
			var conn net.Conn
			conn, err = net.DialTimeout("tcp", tcpAddr, cc.Timeout)
			if err != nil {
				slog.Debug("ssh tcp dial timeout/error", "machine", c.Machine, "addr", tcpAddr, "timeout", cc.Timeout, "err", err)
				return nil, "", "", err
			}
			var (
				ncc   ssh.Conn
				chans <-chan ssh.NewChannel
				reqs  <-chan *ssh.Request
			)
			ncc, chans, reqs, err = ssh.NewClientConn(conn, sshAddr, cc)
			if err != nil {
				_ = conn.Close()
				slog.Debug("ssh NewClientConn failed", "machine", c.Machine, "addr", sshAddr, "err", err)
				return nil, "", "", err
			}
			client = ssh.NewClient(ncc, chans, reqs)
		}
	}
	if err != nil {
		slog.Debug("ssh dial failed", "machine", c.Machine, "err", err)
		return nil, "", "", err
	}

	slog.Debug("ssh connected", "machine", c.Machine, "addr", tcpAddr, "alias", alias)
	startKeepalive(client, cfg, alias)
	return client, string(client.ServerVersion()), targetBanner, nil
}

// tunnelThrough dials targetTCPAddr/targetCC through proxyChain (comma-separated,
// left-to-right as in ssh_config ProxyJump). targetSSHAddr is the alias:port used
// for the SSH handshake so known_hosts is checked by alias, not resolved IP.
func tunnelThrough(cfg *gosshconfig.UserSettings, targetTCPAddr, targetSSHAddr string, targetCC *ssh.ClientConfig, proxyChain, knownHosts string) (*ssh.Client, error) {
	// Split at the last comma: the rightmost entry is directly adjacent to the
	// target; the remainder is the inner chain to reach that host.
	lastJump, innerChain := proxyChain, ""
	if idx := strings.LastIndex(proxyChain, ","); idx != -1 {
		innerChain = strings.TrimSpace(proxyChain[:idx])
		lastJump = strings.TrimSpace(proxyChain[idx+1:])
	}

	slog.Debug("ssh tunnel start", "target", targetTCPAddr, "via", lastJump, "chain", innerChain)

	var (
		jumpClient *ssh.Client
		err        error
	)
	if innerChain == "" {
		// Single hop: use dial() so the jump host's own ssh_config (including
		// its own ProxyJump) is respected.
		// Jump host banner/version not surfaced.
		jumpClient, _, _, err = Config{Machine: lastJump, KnownHosts: knownHosts}.dial()
	} else {
		// Multi-hop: route to lastJump through the remaining inner chain.
		var jumpCC *ssh.ClientConfig
		var jumpTCPAddr, jumpSSHAddr string
		jumpCC, jumpTCPAddr, jumpSSHAddr, err = Config{Machine: lastJump, KnownHosts: knownHosts}.clientConfig(cfg)
		if err != nil {
			slog.Debug("ssh jump clientConfig failed", "jump", lastJump, "err", err)
			return nil, fmt.Errorf("jump host %q: %w", lastJump, err)
		}
		jumpClient, err = tunnelThrough(cfg, jumpTCPAddr, jumpSSHAddr, jumpCC, innerChain, knownHosts)
		if err == nil {
			startKeepalive(jumpClient, cfg, lastJump)
		}
	}
	if err != nil {
		slog.Debug("ssh connecting to jump host failed", "jump", lastJump, "err", err)
		return nil, fmt.Errorf("connecting to jump host %q: %w", lastJump, err)
	}

	conn, err := jumpClient.Dial("tcp", targetTCPAddr)
	if err != nil {
		_ = jumpClient.Close()
		slog.Debug("ssh jump dial failed", "jump", lastJump, "target", targetTCPAddr, "err", err)
		return nil, fmt.Errorf("jump %q -> %q: %w", lastJump, targetTCPAddr, err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(&jumpedConn{conn, jumpClient}, targetSSHAddr, targetCC)
	if err != nil {
		_ = conn.Close()
		_ = jumpClient.Close()
		slog.Debug("ssh NewClientConn (jump) failed", "jump", lastJump, "target", targetSSHAddr, "err", err)
		return nil, err
	}
	slog.Debug("ssh jump connected", "jump", lastJump, "target", targetTCPAddr)
	return ssh.NewClient(ncc, chans, reqs), nil
}

// jumpedConn wraps the TCP channel through a jump host so that closing it also
// closes the underlying jump client connection.
type jumpedConn struct {
	net.Conn
	jump *ssh.Client
}

func (jc *jumpedConn) Close() error {
	err := jc.Conn.Close()
	_ = jc.jump.Close()
	return err
}

// startKeepalive starts a background goroutine that sends OpenSSH-compatible
// keepalive requests at the interval specified by ServerAliveInterval in
// ~/.ssh/config for alias. It closes the client if ServerAliveCountMax
// consecutive failures are observed (default 3, matching OpenSSH).
func startKeepalive(client *ssh.Client, cfg *gosshconfig.UserSettings, alias string) {
	if cfg == nil {
		cfg = &gosshconfig.UserSettings{IgnoreErrors: false}
	}
	intervalStr := cfg.Get(alias, "ServerAliveInterval")
	if intervalStr == "" {
		return
	}
	secs, err := strconv.Atoi(intervalStr)
	if err != nil || secs <= 0 {
		return
	}

	maxCount := 3 // OpenSSH default
	if countStr := cfg.Get(alias, "ServerAliveCountMax"); countStr != "" {
		if n, err := strconv.Atoi(countStr); err == nil && n > 0 {
			maxCount = n
		}
	}

	slog.Debug("ssh keepalive enabled", "alias", alias, "interval_sec", secs, "max_count", maxCount)

	done := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(done)
	}()

	go func() {
		t := time.NewTicker(time.Duration(secs) * time.Second)
		defer t.Stop()
		fails := 0
		for {
			select {
			case <-done:
				return
			case <-t.C:
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				if err != nil {
					fails++
					slog.Debug("ssh keepalive failed", "alias", alias, "fails", fails, "max", maxCount, "err", err)
					if fails >= maxCount {
						slog.Debug("ssh keepalive max failures, closing connection", "alias", alias, "fails", fails)
						_ = client.Close()
						return
					}
				} else {
					fails = 0
				}
			}
		}
	}()
}

// dialProxyCommand executes command (with %h/%p/%r/%% tokens expanded) and
// wraps the subprocess stdin/stdout as a net.Conn for use with ssh.NewClientConn.
func dialProxyCommand(command, addr, remoteUser string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
		portStr = "22"
	}

	expanded := expandProxyCommandTokens(command, host, portStr, remoteUser)
	slog.Debug("ssh proxycommand start", "command", expanded, "addr", addr)

	cmd := exec.Command("sh", "-c", expanded) //nolint:gosec // proxy command is user-controlled, same as OpenSSH ProxyCommand
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("proxy command stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("proxy command stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("proxy command start %q: %w", expanded, err)
	}

	return &proxyCommandConn{stdin: stdin, stdout: stdout, cmd: cmd}, nil
}

func expandProxyCommandTokens(command, host, port, remoteUser string) string {
	var b strings.Builder
	for i := 0; i < len(command); i++ {
		if command[i] == '%' && i+1 < len(command) {
			switch command[i+1] {
			case 'h':
				b.WriteString(host)
				i++
				continue
			case 'p':
				b.WriteString(port)
				i++
				continue
			case 'r':
				b.WriteString(remoteUser)
				i++
				continue
			case '%':
				b.WriteByte('%')
				i++
				continue
			}
		}
		b.WriteByte(command[i])
	}
	return b.String()
}

type proxyCommandConn struct {
	stdin  interface{ Write([]byte) (int, error) }
	stdout interface{ Read([]byte) (int, error) }
	cmd    *exec.Cmd
}

func (c *proxyCommandConn) Read(b []byte) (int, error)  { return c.stdout.Read(b) }
func (c *proxyCommandConn) Write(b []byte) (int, error) { return c.stdin.Write(b) }
func (c *proxyCommandConn) Close() error {
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
	return nil
}
func (c *proxyCommandConn) LocalAddr() net.Addr              { return proxyCommandAddr{} }
func (c *proxyCommandConn) RemoteAddr() net.Addr             { return proxyCommandAddr{} }
func (c *proxyCommandConn) SetDeadline(time.Time) error      { return nil }
func (c *proxyCommandConn) SetReadDeadline(time.Time) error  { return nil }
func (c *proxyCommandConn) SetWriteDeadline(time.Time) error { return nil }

type proxyCommandAddr struct{}

func (proxyCommandAddr) Network() string { return "proxy-command" }
func (proxyCommandAddr) String() string  { return "proxy-command" }

func parseTarget(target string) (usr, host string, port int) {
	if target == "" {
		return "", "", 0
	}
	s := target
	if at := strings.LastIndex(s, "@"); at != -1 {
		usr = s[:at]
		s = s[at+1:]
	}
	if strings.HasPrefix(s, "[") {
		if idx := strings.LastIndex(s, "]:"); idx != -1 {
			host = s[1:idx]
			if p, err := strconv.Atoi(s[idx+2:]); err == nil {
				port = p
			}
			return
		}
		host = strings.Trim(s, "[]")
		return
	}
	if idx := strings.LastIndex(s, ":"); idx != -1 {
		host = s[:idx]
		if p, err := strconv.Atoi(s[idx+1:]); err == nil {
			port = p
		}
		return
	}
	host = s
	return
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "root"
}

func homeDir() string {
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return "/root"
}

func hostKeyCallback(kh string) ssh.HostKeyCallback {
	if kh == "insecure" {
		slog.Warn("ssh host key verification disabled (insecure mode)")
		return ssh.InsecureIgnoreHostKey() //nolint:gosec // explicitly requested by user via KnownHosts=insecure
	}
	path := kh
	if path == "" {
		path = filepath.Join(homeDir(), ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		if os.IsNotExist(err) {
			dir := filepath.Dir(path)
			_ = os.MkdirAll(dir, 0o700)
			//nolint:gosec // known_hosts is not secret (like OpenSSH)
			if f, err2 := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644); err2 == nil {
				_ = f.Close()
			}
			cb, err = knownhosts.New(path)
		}
	}
	if err != nil {
		slog.Error("failed to load known_hosts", "path", path, "err", err)
		loadErr := err
		return func(_ string, _ net.Addr, _ ssh.PublicKey) error {
			return fmt.Errorf("known_hosts unavailable: %w", loadErr)
		}
	}
	return cb
}

// multiAddrHostKeyCallback returns a HostKeyCallback that checks addresses[0]
// (the alias) first, then falls back to addresses[1:] (resolved IPs).
//
// A key-changed error on the alias is always fatal (possible MITM). A
// key-changed error on a fallback address is only a warning — stale IP entries
// in known_hosts are common and should not block a new connection whose alias
// has no entry yet. If all addresses are unknown the error names all of them so
// the user knows exactly what to add.
func multiAddrHostKeyCallback(kh string, addresses ...string) ssh.HostKeyCallback {
	rawCB := hostKeyCallback(kh)
	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		for i, addr := range addresses {
			slog.Debug("checking host key", "addr", addr)
			err := rawCB(addr, remote, key)
			if err == nil {
				return nil
			}
			var keyErr *knownhosts.KeyError
			if errors.As(err, &keyErr) && len(keyErr.Want) > 0 {
				if i == 0 {
					// Alias entry exists but key changed — treat as hard failure.
					return fmt.Errorf("host key changed for %s: %w", addr, err)
				}
				// Stale fallback (IP) entry — log and skip rather than failing.
				slog.Warn("stale known_hosts entry, skipping", "addr", addr)
				continue
			}
		}
		// addresses[0] is the alias (not necessarily DNS-resolvable by ssh-keyscan);
		// addresses[1] is the resolved IP. Use the last address for the keyscan
		// suggestion since it's always a real address.
		scanHost := knownhosts.Normalize(addresses[len(addresses)-1])
		return fmt.Errorf("host key unknown — add with: ssh-keyscan -H %s >> ~/.ssh/known_hosts", scanHost)
	}
}

func authMethods(cfg *gosshconfig.UserSettings, c Config, alias, _ string) ([]ssh.AuthMethod, error) {
	if cfg == nil {
		cfg = &gosshconfig.UserSettings{IgnoreErrors: false}
	}
	var m []ssh.AuthMethod
	h := homeDir()

	if c.KeyPath != "" {
		key, err := os.ReadFile(c.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("reading key %q: %w", c.KeyPath, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			if p, ok := tryParseWithPass(c.Password, key); ok {
				return []ssh.AuthMethod{ssh.PublicKeys(p)}, nil
			}
			return nil, fmt.Errorf("parsing key %q: %w", c.KeyPath, err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}

	if c.Password != "" {
		m = append(m, ssh.Password(c.Password), ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range questions {
				answers[i] = c.Password
			}
			return answers, nil
		}))
	}

	identitiesOnly := strings.EqualFold(cfg.Get(alias, "IdentitiesOnly"), "yes")
	agentSock := resolveAgentSock(cfg, alias, h)

	// IdentityFile paths from ssh_config (may include .pub files).
	var cfgPaths []string
	for _, p := range cfg.GetAll(alias, "IdentityFile") {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(h, rest)
		}
		cfgPaths = append(cfgPaths, p)
	}

	// Determine which paths to try. IdentitiesOnly restricts to cfgPaths
	// only when there are explicit entries; otherwise fall back to defaults.
	keyPaths := buildKeyPaths(cfgPaths, h, identitiesOnly)

	// First pass: collect allowed public keys (for agent filtering) and file signers.
	var (
		allowedPubs []ssh.PublicKey
		fileSigners []ssh.AuthMethod
	)
	for _, p := range keyPaths {
		pk, signer := loadKeyFile(p, c.Password)
		if signer != nil {
			fileSigners = append(fileSigners, ssh.PublicKeys(signer))
		}
		if pk != nil {
			allowedPubs = append(allowedPubs, pk)
		}
	}
	if identitiesOnly && len(cfgPaths) > 0 && len(allowedPubs) == 0 {
		slog.Warn("ssh auth: IdentitiesOnly is set but none of the configured IdentityFile paths could be loaded; "+
			"agent keys will not be offered (key may exist only in agent, not on disk)",
			"alias", alias,
			"identity_files", cfgPaths,
		)
	}

	// Agent: always consulted unless socket is empty. With IdentitiesOnly, only
	// keys whose public key matches an IdentityFile entry are offered.
	if agentSock != "" {
		if conn, err := net.Dial("unix", agentSock); err == nil {
			ac := agent.NewClient(conn)
			if identitiesOnly && len(allowedPubs) > 0 {
				m = append(m, ssh.PublicKeysCallback(filteredAgentSigners(ac, allowedPubs)))
			} else if !identitiesOnly {
				m = append(m, ssh.PublicKeysCallback(ac.Signers))
			}
		}
	}

	m = append(m, fileSigners...)
	if len(m) == 0 {
		return nil, fmt.Errorf("no authentication methods available for %q: "+
			"no password set, no readable key files found, and no SSH agent keys offered "+
			"(if IdentitiesOnly is set, ensure the IdentityFile exists on disk or remove IdentitiesOnly)", alias)
	}
	return m, nil
}

// buildKeyPaths returns the ordered, deduplicated list of key file paths to try.
// cfgPaths come from IdentityFile in ssh_config. When identitiesOnly is true and
// cfgPaths is non-empty, only those paths are returned; otherwise the standard
// default key names are appended.
func buildKeyPaths(cfgPaths []string, home string, identitiesOnly bool) []string {
	if identitiesOnly && len(cfgPaths) > 0 {
		return cfgPaths
	}
	seen := make(map[string]struct{}, len(cfgPaths)+3)
	out := make([]string, 0, len(cfgPaths)+3)
	for _, p := range cfgPaths {
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		p := filepath.Join(home, ".ssh", name)
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

// resolveAgentSock returns the SSH agent socket path for alias from ssh_config.
func resolveAgentSock(cfg *gosshconfig.UserSettings, alias, home string) string {
	if cfg == nil {
		cfg = &gosshconfig.UserSettings{IgnoreErrors: false}
	}
	switch ia := cfg.Get(alias, "IdentityAgent"); strings.ToLower(ia) {
	case "", "ssh_auth_sock":
		return os.Getenv("SSH_AUTH_SOCK")
	case "none":
		return ""
	default:
		if rest, ok := strings.CutPrefix(ia, "~/"); ok {
			return filepath.Join(home, rest)
		}
		return ia
	}
}

// loadKeyFile tries to load a key from path. It handles both private key files
// and public-key files in authorized_keys format (e.g. ~/.ssh/id_ed25519.pub).
// Returns (publicKey, signer): signer is nil for .pub-only files.
// Returns (nil, nil) silently when the file does not exist; logs a warning when
// the file exists but cannot be parsed as either format.
func loadKeyFile(path, pass string) (ssh.PublicKey, ssh.Signer) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from ssh config or explicit KeyPath, user-controlled
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("reading identity file", "path", path, "err", err)
		}
		return nil, nil
	}
	if s, err := ssh.ParsePrivateKey(data); err == nil {
		return s.PublicKey(), s
	}
	if s, ok := tryParseWithPass(pass, data); ok {
		return s.PublicKey(), s
	}
	// Fall back to authorized_keys / .pub format (public key only).
	if pk, _, _, _, err := ssh.ParseAuthorizedKey(data); err == nil {
		return pk, nil
	}
	slog.Warn("identity file is not a recognized private key or public key format", "path", path)
	return nil, nil
}

// filteredAgentSigners returns a Signers callback that only yields signers from
// ac whose public key matches one of the allowed keys. Used for IdentitiesOnly.
func filteredAgentSigners(ac agent.ExtendedAgent, allowed []ssh.PublicKey) func() ([]ssh.Signer, error) {
	return func() ([]ssh.Signer, error) {
		all, err := ac.Signers()
		if err != nil {
			return nil, err
		}
		var out []ssh.Signer
		for _, s := range all {
			for _, pk := range allowed {
				if bytes.Equal(s.PublicKey().Marshal(), pk.Marshal()) {
					out = append(out, s)
					break
				}
			}
		}
		return out, nil
	}
}

func tryParseWithPass(pass string, key []byte) (ssh.Signer, bool) {
	if pass == "" {
		return nil, false
	}
	if s, err := ssh.ParsePrivateKeyWithPassphrase(key, []byte(pass)); err == nil {
		return s, true
	}
	return nil, false
}
