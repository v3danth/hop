package main

import (
	"context"
	"crypto"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// serveConfig is what hop serve runs with.
type serveConfig struct {
	addr      string // plain HTTP address; hosts entries point at its IP
	tlsAddr   string // HTTPS address; empty turns HTTPS off
	hostsPath string // hosts file to keep in sync; empty leaves it alone
	stateDir  string // holds registry.json and the local CA
}

func serveCmd(args []string) error {
	cfg, err := parseServeFlags(args)
	if err != nil {
		return err
	}
	d, err := openDaemon(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return d.run(ctx)
}

func parseServeFlags(args []string) (serveConfig, error) {
	var cfg serveConfig
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fs.StringVar(&cfg.addr, "addr", defaultAddr, "HTTP address, on a loopback IP; hosts entries point at that IP")
	fs.StringVar(&cfg.tlsAddr, "tls-addr", "", `HTTPS address (default: the -addr IP, port 443; "off" turns HTTPS off)`)
	fs.StringVar(&cfg.hostsPath, "hosts", "", "hosts file to keep in sync, e.g. /etc/hosts, so single-word names like yt/ resolve")
	fs.StringVar(&cfg.stateDir, "state", "", "directory for the registry and the local CA (default ~/.hop)")
	if err := fs.Parse(args); err != nil {
		return serveConfig{}, err
	}
	if fs.NArg() > 0 {
		return serveConfig{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ip, _, err := splitLoopback(cfg.addr)
	if err != nil {
		return serveConfig{}, fmt.Errorf("-addr: %w", err)
	}
	switch cfg.tlsAddr {
	case "off":
		cfg.tlsAddr = ""
	case "":
		cfg.tlsAddr = net.JoinHostPort(ip, "443")
	}
	if cfg.stateDir == "" {
		// launchd and systemd services have no $HOME, so look it up only
		// when -state is missing.
		home, err := os.UserHomeDir()
		if err != nil {
			return serveConfig{}, fmt.Errorf("find home directory (or pass -state): %w", err)
		}
		cfg.stateDir = filepath.Join(home, ".hop")
	}
	return cfg, nil
}

// daemon is hop serve with its state loaded and its listeners open.
type daemon struct {
	cfg     serveConfig
	hostsIP string
	reg     *Registry
	srv     *Server
	http    *http.Server
	ln      net.Listener // plain HTTP
	tlsLn   net.Listener // nil when HTTPS is off
}

func openDaemon(cfg serveConfig) (d *daemon, err error) {
	d = &daemon{cfg: cfg}
	ip, httpPort, err := splitLoopback(cfg.addr)
	if err != nil {
		return nil, fmt.Errorf("-addr: %w", err)
	}
	d.hostsIP = ip
	// 0700: the directory holds the CA key, and anyone with that key can
	// impersonate any website to this machine.
	if err := os.MkdirAll(cfg.stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if d.reg, err = loadRegistry(filepath.Join(cfg.stateDir, "registry.json")); err != nil {
		return nil, err
	}
	d.reg.ReapDead()

	links := linkStyle{shortNames: cfg.hostsPath != "", httpPort: httpPort}
	var (
		caCert *x509.Certificate
		caKey  crypto.Signer
	)
	if cfg.tlsAddr != "" {
		if _, links.httpsPort, err = splitLoopback(cfg.tlsAddr); err != nil {
			return nil, fmt.Errorf("-tls-addr: %w", err)
		}
		if caCert, caKey, err = loadOrCreateCA(cfg.stateDir); err != nil {
			return nil, err
		}
	}
	d.srv = newServer(d.reg, links)
	d.http = &http.Server{
		Handler:           d.srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Failed TLS handshakes, such as a client that does not trust the
		// CA, are routine; keep them out of the log unless debugging.
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelDebug),
	}

	if d.ln, err = listenLoopback(cfg.addr); err != nil {
		return nil, err
	}
	if cfg.tlsAddr != "" {
		d.http.TLSConfig = newCertIssuer(caCert, caKey, d.srv.servesName).tlsConfig()
		if d.tlsLn, err = listenLoopback(cfg.tlsAddr); err != nil {
			d.ln.Close()
			return nil, err
		}
	}
	return d, nil
}

// run serves until ctx is cancelled or a listener fails. It then lets
// in-flight requests finish and stops the maintenance loop before the final
// save, so no change made during shutdown is lost.
func (d *daemon) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	maintained := make(chan struct{})
	go func() {
		defer close(maintained)
		d.srv.maintain(ctx, d.cfg.hostsPath, d.hostsIP)
	}()

	failed := make(chan error, 2)
	go func() { failed <- d.http.Serve(d.ln) }()
	if d.tlsLn != nil {
		go func() { failed <- d.http.ServeTLS(d.tlsLn, "", "") }()
	}
	slog.Info("hop listening", "http", d.ln.Addr(), "https", d.cfg.tlsAddr, "state", d.cfg.stateDir,
		"hosts", d.cfg.hostsPath, "dashboard", d.srv.links.url("http", "hop")+"/")

	var err error
	select {
	case <-ctx.Done():
	case serveErr := <-failed:
		err = fmt.Errorf("serve: %w", serveErr)
	}
	shutdownCtx, stopShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopShutdown()
	if serr := d.http.Shutdown(shutdownCtx); serr != nil {
		slog.Warn("shutdown cut open connections", "err", serr)
	}
	cancel()
	<-maintained
	d.reg.SaveIfDirty()
	slog.Info("hop stopped")
	return err
}

// splitLoopback splits addr into its IP and port. hop serves this machine
// only, so the IP must be a loopback one; a wildcard address would expose
// the API and every local app to the network.
func splitLoopback(addr string) (ip string, port int, err error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	parsed := net.ParseIP(host)
	if parsed == nil || !parsed.IsLoopback() {
		return "", 0, fmt.Errorf("%q must use a loopback IP such as %s", addr, defaultIP)
	}
	port, err = strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return "", 0, fmt.Errorf("%q has an invalid port", addr)
	}
	return parsed.String(), port, nil
}

func listenLoopback(addr string) (net.Listener, error) {
	if _, _, err := splitLoopback(addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w%s", addr, err, listenHint(addr, err))
	}
	return ln, nil
}

// listenHint explains the listen failures people actually run into.
func listenHint(addr string, err error) string {
	host, port, _ := net.SplitHostPort(addr)
	switch {
	case errors.Is(err, syscall.EADDRNOTAVAIL) && runtime.GOOS == "darwin":
		return fmt.Sprintf(" (macOS only enables 127.0.0.1; run `sudo ifconfig lo0 alias %s up`)", host)
	case errors.Is(err, syscall.EACCES):
		return " (ports below 1024 need root)"
	case errors.Is(err, syscall.EADDRINUSE):
		// Linux, unlike macOS, refuses a specific address while another
		// server listens on every address (0.0.0.0) with the same port.
		return fmt.Sprintf(" (%s shows what holds it; on Linux that includes servers listening on 0.0.0.0:%s)", portOwnerCommand(port), port)
	}
	return ""
}

func portOwnerCommand(port string) string {
	if runtime.GOOS == "linux" {
		return "`sudo ss -ltnp 'sport = :" + port + "'`"
	}
	return "`sudo lsof -nP -iTCP:" + port + " -sTCP:LISTEN`"
}
