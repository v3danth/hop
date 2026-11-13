package main

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSplitLoopback(t *testing.T) {
	tests := []struct {
		addr     string
		wantIP   string
		wantPort int
		wantErr  bool
	}{
		{"127.0.0.2:80", "127.0.0.2", 80, false},
		{"[::1]:443", "::1", 443, false},
		{"127.0.0.1:0", "127.0.0.1", 0, false},
		{"0.0.0.0:80", "", 0, true}, // would expose hop to the network
		{"192.168.1.5:80", "", 0, true},
		{"localhost:80", "", 0, true},
		{"127.0.0.2", "", 0, true},
		{"127.0.0.2:http", "", 0, true},
		{"127.0.0.2:70000", "", 0, true},
	}
	for _, tt := range tests {
		ip, port, err := splitLoopback(tt.addr)
		if ip != tt.wantIP || port != tt.wantPort || (err != nil) != tt.wantErr {
			t.Errorf("splitLoopback(%q) = %q, %d, %v; want %q, %d, wantErr %v", tt.addr, ip, port, err, tt.wantIP, tt.wantPort, tt.wantErr)
		}
	}
}

func TestParseServeFlags(t *testing.T) {
	cfg, err := parseServeFlags([]string{"-addr", "127.0.0.3:8080", "-state", "/tmp/hop-state"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.tlsAddr != "127.0.0.3:443" {
		t.Errorf("default tls address = %q, want the -addr IP on 443", cfg.tlsAddr)
	}
	cfg, err = parseServeFlags([]string{"-tls-addr", "off", "-state", "/tmp/hop-state"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.tlsAddr != "" || cfg.addr != defaultAddr {
		t.Errorf("cfg = %+v, want HTTPS off on %s", cfg, defaultAddr)
	}
	if _, err := parseServeFlags([]string{"-addr", "0.0.0.0:80"}); err == nil {
		t.Error("accepted a wildcard address")
	}
}

func TestListenExplainsAPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = listenLoopback(ln.Addr().String())
	if !errors.Is(err, syscall.EADDRINUSE) || !strings.Contains(err.Error(), "shows what holds it") {
		t.Errorf("listenLoopback on a busy port: %v", err)
	}
}

// TestDaemonServesAndSavesOnShutdown runs hop serve end to end on spare
// ports: a name added over HTTP must be on disk after a clean stop, and
// HTTPS must work with the CA in the state directory.
func TestDaemonServesAndSavesOnShutdown(t *testing.T) {
	state := t.TempDir()
	d, err := openDaemon(serveConfig{addr: "127.0.0.1:0", tlsAddr: "127.0.0.1:0", stateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- d.run(ctx) }()

	c := client{base: "http://" + d.ln.Addr().String(), http: &http.Client{Timeout: 5 * time.Second}}
	if _, err := c.put(Service{Name: "zz", URL: "https://example.com"}, false); err != nil {
		t.Fatalf("add over HTTP: %v", err)
	}

	caPEM, err := os.ReadFile(filepath.Join(state, caCertFile))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	resp, err := pinnedClient(d.tlsLn.Addr().String(), roots).Get("https://zz/")
	if err != nil {
		t.Fatalf("https://zz: %v", err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "https://example.com" {
		t.Errorf("https://zz redirected to %q", loc)
	}

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	saved, err := loadRegistry(filepath.Join(state, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Lookup("zz"); !ok {
		t.Error("name added before shutdown was not saved")
	}
	if conn, err := net.Dial("tcp", d.ln.Addr().String()); err == nil {
		conn.Close()
		t.Error("still listening after shutdown")
	}
}
