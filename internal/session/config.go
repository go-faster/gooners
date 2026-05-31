package session

import (
	"fmt"
	"log"
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

func (c Config) clientConfig() (*ssh.ClientConfig, string, error) {
	usr, alias, port := parseTarget(c.Machine)
	if alias == "" {
		alias = c.Machine
	}

	// alias is the original hostname before HostName resolution — used for all
	// ssh_config lookups, matching OpenSSH semantics.
	host := alias
	if cfgHostname := gosshconfig.Get(alias, "HostName"); cfgHostname != "" && cfgHostname != alias {
		host = cfgHostname
	}
	if usr == "" {
		if cfgUser := gosshconfig.Get(alias, "User"); cfgUser != "" {
			usr = cfgUser
		}
	}
	if port == 0 {
		if cfgPort := gosshconfig.Get(alias, "Port"); cfgPort != "" {
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
	addr := fmt.Sprintf("%s:%d", host, port)

	timeout := 30 * time.Second
	if c.TimeoutSec > 0 {
		if c.TimeoutSec > 3600 {
			c.TimeoutSec = 3600
		}
		timeout = time.Duration(c.TimeoutSec) * time.Second
	}

	hkcb := hostKeyCallback(c.KnownHosts)

	auth, err := authMethods(c, alias, usr)
	if err != nil {
		return nil, "", err
	}

	return &ssh.ClientConfig{
		User:            usr,
		Auth:            auth,
		HostKeyCallback: hkcb,
		Timeout:         timeout,
	}, addr, nil
}

// dial opens an SSH connection to c.Machine, following ProxyJump / ProxyCommand
// directives from ~/.ssh/config (unless overridden by Config fields).
func (c Config) dial() (*ssh.Client, error) {
	cc, addr, err := c.clientConfig()
	if err != nil {
		return nil, err
	}

	_, alias, _ := parseTarget(c.Machine)
	if alias == "" {
		alias = c.Machine
	}

	jump := c.ProxyJump
	if jump == "" {
		jump = strings.TrimSpace(gosshconfig.Get(alias, "ProxyJump"))
	}

	var client *ssh.Client
	switch {
	case jump != "" && jump != "none":
		client, err = tunnelThrough(addr, cc, jump, c.KnownHosts)
	default:
		// Fall back to ProxyCommand if set.
		proxyCmd := gosshconfig.Get(alias, "ProxyCommand")
		if proxyCmd != "" && proxyCmd != "none" {
			conn, cerr := dialProxyCommand(proxyCmd, addr, cc.User)
			if cerr != nil {
				return nil, cerr
			}
			ncc, chans, reqs, cerr := ssh.NewClientConn(conn, addr, cc)
			if cerr != nil {
				_ = conn.Close()
				return nil, cerr
			}
			client = ssh.NewClient(ncc, chans, reqs)
		} else {
			client, err = ssh.Dial("tcp", addr, cc)
		}
	}
	if err != nil {
		return nil, err
	}

	startKeepalive(client, alias)
	return client, nil
}

// tunnelThrough dials targetAddr/targetCC through proxyChain, a comma-separated
// list of jump hosts ordered left-to-right (same as ssh_config ProxyJump).
// knownHosts is propagated to each jump hop so the same verification policy applies.
func tunnelThrough(targetAddr string, targetCC *ssh.ClientConfig, proxyChain, knownHosts string) (*ssh.Client, error) {
	// Split at the last comma: the rightmost entry is the host directly adjacent
	// to the target; the rest is the inner chain to reach that host.
	lastJump, innerChain := proxyChain, ""
	if idx := strings.LastIndex(proxyChain, ","); idx != -1 {
		innerChain = strings.TrimSpace(proxyChain[:idx])
		lastJump = strings.TrimSpace(proxyChain[idx+1:])
	}

	var (
		jumpClient *ssh.Client
		err        error
	)
	if innerChain == "" {
		// Single hop: use Config.dial() so the jump host's own ssh_config entries
		// (including its own ProxyJump) are respected.
		jumpClient, err = Config{Machine: lastJump, KnownHosts: knownHosts}.dial()
	} else {
		// Multi-hop: route to lastJump through the remaining inner chain.
		var jumpCC *ssh.ClientConfig
		var jumpAddr string
		jumpCC, jumpAddr, err = Config{Machine: lastJump, KnownHosts: knownHosts}.clientConfig()
		if err != nil {
			return nil, fmt.Errorf("jump host %q: %w", lastJump, err)
		}
		jumpClient, err = tunnelThrough(jumpAddr, jumpCC, innerChain, knownHosts)
	}
	if err != nil {
		return nil, fmt.Errorf("connecting to jump host %q: %w", lastJump, err)
	}

	conn, err := jumpClient.Dial("tcp", targetAddr)
	if err != nil {
		_ = jumpClient.Close()
		return nil, fmt.Errorf("jump %q -> %q: %w", lastJump, targetAddr, err)
	}

	ncc, chans, reqs, err := ssh.NewClientConn(&jumpedConn{conn, jumpClient}, targetAddr, targetCC)
	if err != nil {
		_ = conn.Close()
		_ = jumpClient.Close()
		return nil, err
	}
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
func startKeepalive(client *ssh.Client, alias string) {
	intervalStr := gosshconfig.Get(alias, "ServerAliveInterval")
	if intervalStr == "" {
		return
	}
	secs, err := strconv.Atoi(intervalStr)
	if err != nil || secs <= 0 {
		return
	}

	maxCount := 3 // OpenSSH default
	if countStr := gosshconfig.Get(alias, "ServerAliveCountMax"); countStr != "" {
		if n, err := strconv.Atoi(countStr); err == nil && n > 0 {
			maxCount = n
		}
	}

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
					if fails >= maxCount {
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
		// addr might not have a port when called with bare hostnames.
		host = addr
		portStr = "22"
	}

	expanded := expandProxyCommandTokens(command, host, portStr, remoteUser)

	cmd := exec.Command("sh", "-c", expanded)
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

func expandProxyCommandTokens(command, host, port, user string) string {
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
				b.WriteString(user)
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
		log.Print("WARNING: ssh host key verification disabled (insecure mode)")
		return ssh.InsecureIgnoreHostKey()
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
			if f, err2 := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644); err2 == nil {
				_ = f.Close()
			}
			cb, err = knownhosts.New(path)
		}
	}
	if err != nil {
		log.Printf("error: failed to load known_hosts %s: %v", path, err)
		loadErr := err
		return func(_ string, _ net.Addr, _ ssh.PublicKey) error {
			return fmt.Errorf("known_hosts unavailable: %w", loadErr)
		}
	}
	return cb
}

func authMethods(c Config, alias string, _ string) ([]ssh.AuthMethod, error) {
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
		m = append(m, ssh.Password(c.Password))
		m = append(m, ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range questions {
				answers[i] = c.Password
			}
			return answers, nil
		}))
	}

	// IdentitiesOnly: if set, skip agent and default key fallbacks entirely.
	identitiesOnly := strings.EqualFold(gosshconfig.Get(alias, "IdentitiesOnly"), "yes")

	if !identitiesOnly {
		// Determine which agent socket to use.
		agentSock := ""
		switch ia := gosshconfig.Get(alias, "IdentityAgent"); strings.ToLower(ia) {
		case "", "ssh_auth_sock":
			agentSock = os.Getenv("SSH_AUTH_SOCK")
		case "none":
			// explicitly disabled
		default:
			if rest, ok := strings.CutPrefix(ia, "~/"); ok {
				agentSock = filepath.Join(h, rest)
			} else {
				agentSock = ia
			}
		}
		if agentSock != "" {
			if conn, err := net.Dial("unix", agentSock); err == nil {
				m = append(m, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
			}
		}
	}

	// Collect key paths: IdentityFile from ssh_config first, then defaults
	// (unless IdentitiesOnly restricts us to only ssh_config keys).
	keyPaths := identityFilesFromConfig(alias, h, identitiesOnly)
	for _, p := range keyPaths {
		if key, err := os.ReadFile(p); err == nil {
			if signer, err := ssh.ParsePrivateKey(key); err == nil {
				m = append(m, ssh.PublicKeys(signer))
			} else if pkey, ok := tryParseWithPass(c.Password, key); ok {
				m = append(m, ssh.PublicKeys(pkey))
			}
		}
	}
	return m, nil
}

// identityFilesFromConfig returns key paths to try: IdentityFile entries from
// ssh_config for alias first, then the standard defaults (unless identitiesOnly).
// Duplicate paths are deduplicated.
func identityFilesFromConfig(alias, home string, identitiesOnly bool) []string {
	seen := make(map[string]struct{})
	var out []string

	add := func(p string) {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(home, rest)
		}
		if _, ok := seen[p]; !ok {
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}

	for _, p := range gosshconfig.GetAll(alias, "IdentityFile") {
		add(p)
	}
	if !identitiesOnly {
		for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
			add(filepath.Join(home, ".ssh", name))
		}
	}
	return out
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
