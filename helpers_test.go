package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	reg, err := loadRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func mustPut(t *testing.T, reg *Registry, svcs ...Service) {
	t.Helper()
	for _, svc := range svcs {
		if _, err := reg.Put(svc, true); err != nil {
			t.Fatal(err)
		}
	}
}

// portOf returns the port of a listener address like 127.0.0.1:1234.
func portOf(t *testing.T, addr net.Addr) int {
	t.Helper()
	_, portText, err := net.SplitHostPort(addr.String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// newTestServer returns hop with a website shortcut yt and a local app
// named app, which answers every path with "app:" plus the path.
func newTestServer(t *testing.T) (*Server, int) {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "app:"+r.URL.Path)
	}))
	t.Cleanup(app.Close)
	port := portOf(t, app.Listener.Addr())
	reg := newTestRegistry(t)
	mustPut(t, reg, Service{Name: "yt", URL: "https://youtube.com"}, Service{Name: "app", Port: port})
	return newServer(reg, linkStyle{shortNames: true, httpPort: 80, httpsPort: 443}), port
}

// serve sends a request for target with the given Host header to s. A target
// starting with https:// arrives as if over TLS.
func serve(s *Server, host, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func newTestIssuer(t *testing.T, names ...string) (*certIssuer, *x509.CertPool) {
	t.Helper()
	caCert, caKey, err := loadOrCreateCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return newCertIssuer(caCert, caKey, func(n string) bool { return slices.Contains(names, n) }), pool
}

// pinnedClient sends every request to addr, whatever its host, the way
// /etc/hosts sends hop's names to hop, and does not follow redirects.
func pinnedClient(addr string, roots *x509.CertPool) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
