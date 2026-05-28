package session

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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
}

func (c Config) clientConfig() (*ssh.ClientConfig, string, error) {
	usr, host, port := parseTarget(c.Machine)
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
	if host == "" {
		host = c.Machine
	}
	addr := fmt.Sprintf("%s:%d", host, port)

	timeout := 30 * time.Second
	if c.TimeoutSec > 0 {
		timeout = time.Duration(c.TimeoutSec) * time.Second
	}

	hkcb := hostKeyCallback(c.KnownHosts)

	auth := authMethods(c)

	return &ssh.ClientConfig{
		User:            usr,
		Auth:            auth,
		HostKeyCallback: hkcb,
		Timeout:         timeout,
	}, addr, nil
}

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

func authMethods(c Config) []ssh.AuthMethod {
	var m []ssh.AuthMethod
	h := homeDir()

	if c.KeyPath != "" {
		if key, err := os.ReadFile(c.KeyPath); err == nil {
			if signer, err := ssh.ParsePrivateKey(key); err == nil {
				m = append(m, ssh.PublicKeys(signer))
			} else if p, ok := tryParseWithPass(c.Password, key); ok {
				m = append(m, ssh.PublicKeys(p))
			}
		}
		return m
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

	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			m = append(m, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		p := filepath.Join(h, ".ssh", name)
		if key, err := os.ReadFile(p); err == nil {
			if signer, err := ssh.ParsePrivateKey(key); err == nil {
				m = append(m, ssh.PublicKeys(signer))
			} else if pkey, ok := tryParseWithPass(c.Password, key); ok {
				m = append(m, ssh.PublicKeys(pkey))
			}
		}
	}
	return m
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
