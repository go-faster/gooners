package session

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"

	gosshconfig "github.com/kevinburke/ssh_config"
	"golang.org/x/crypto/ssh"
)

type localForward struct {
	listenAddr string
	targetAddr string
}

func startLocalForwards(client *ssh.Client, cfg *gosshconfig.UserSettings, alias, home string, logger *slog.Logger) ([]io.Closer, error) {
	if cfg == nil {
		cfg = newSettings(home)
	}
	var closers []io.Closer
	for _, raw := range cfg.GetAll(alias, "LocalForward") {
		fwd, err := parseLocalForward(raw)
		if err != nil {
			closeAll(closers)
			return nil, err
		}
		ln, err := net.Listen("tcp", fwd.listenAddr)
		if err != nil {
			closeAll(closers)
			return nil, fmt.Errorf("local forward listen %s: %w", fwd.listenAddr, err)
		}
		closers = append(closers, ln)
		logger.Info("ssh local forward listening", "alias", alias, "listen", ln.Addr().String(), "target", fwd.targetAddr)
		go serveLocalForward(client, ln, fwd.targetAddr, logger)
	}
	return closers, nil
}

func parseLocalForward(raw string) (localForward, error) {
	fields := strings.Fields(raw)
	if len(fields) != 2 {
		return localForward{}, fmt.Errorf("unsupported LocalForward %q: expected '[bind_address:]port host:hostport'", raw)
	}

	listenAddr, err := parseForwardListenAddr(fields[0])
	if err != nil {
		return localForward{}, fmt.Errorf("local forward listen address: %w", err)
	}
	targetAddr, err := parseForwardTargetAddr(fields[1])
	if err != nil {
		return localForward{}, fmt.Errorf("local forward target address: %w", err)
	}
	return localForward{listenAddr: listenAddr, targetAddr: targetAddr}, nil
}

func parseForwardListenAddr(spec string) (string, error) {
	if _, err := strconv.Atoi(spec); err == nil {
		return net.JoinHostPort("127.0.0.1", spec), nil
	}
	host, port, err := splitForwardAddr(spec)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

func parseForwardTargetAddr(spec string) (string, error) {
	host, port, err := splitForwardAddr(spec)
	if err != nil {
		return "", err
	}
	if host == "" {
		return "", fmt.Errorf("missing target host in %q", spec)
	}
	return net.JoinHostPort(host, port), nil
}

func splitForwardAddr(spec string) (host, port string, err error) {
	if h, p, err := net.SplitHostPort(spec); err == nil {
		return h, p, nil
	}
	idx := strings.LastIndex(spec, ":")
	if idx == -1 {
		return "", "", fmt.Errorf("missing port in %q", spec)
	}
	host, port = spec[:idx], spec[idx+1:]
	if port == "" {
		return "", "", fmt.Errorf("missing port in %q", spec)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", fmt.Errorf("invalid port %q", port)
	}
	return strings.Trim(host, "[]"), port, nil
}

func serveLocalForward(client *ssh.Client, ln net.Listener, targetAddr string, logger *slog.Logger) {
	for {
		localConn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleLocalForwardConn(client, localConn, targetAddr, logger)
	}
}

func handleLocalForwardConn(client *ssh.Client, localConn net.Conn, targetAddr string, logger *slog.Logger) {
	defer func() { _ = localConn.Close() }()
	remoteConn, err := client.Dial("tcp", targetAddr)
	if err != nil {
		logger.Debug("ssh local forward dial failed", "target", targetAddr, "err", err)
		return
	}
	defer func() { _ = remoteConn.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remoteConn, localConn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(localConn, remoteConn)
		done <- struct{}{}
	}()
	<-done
}

func closeAll(closers []io.Closer) {
	for _, c := range closers {
		_ = c.Close()
	}
}
