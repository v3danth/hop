package main

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreateCAIsStable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	first, _, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := loadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(second) {
		t.Error("second start made a new CA; the trusted one would stop working")
	}
	if !first.IsCA || first.Subject.CommonName != caCommonName {
		t.Errorf("CA = %+v", first.Subject)
	}
	info, err := os.Stat(filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("CA key mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestCertIssuerSignsOnlyServedNames(t *testing.T) {
	ci, pool := newTestIssuer(t, "yt", "hop")
	tests := []struct {
		serverName, verifyAs string
		wantErr              bool
	}{
		{"yt", "yt", false},
		{"yt.localhost", "yt.localhost", false},
		{"YT.", "yt", false},
		{"", "hop", false}, // connections by IP send no name
		{"google.com", "", true},
		{"nope", "", true},
	}
	for _, tt := range tests {
		c, err := ci.certificate(&tls.ClientHelloInfo{ServerName: tt.serverName})
		if (err != nil) != tt.wantErr {
			t.Errorf("certificate(%q) error = %v, wantErr %v", tt.serverName, err, tt.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if _, err := c.Leaf.Verify(x509.VerifyOptions{DNSName: tt.verifyAs, Roots: pool}); err != nil {
			t.Errorf("certificate(%q) does not verify as %q: %v", tt.serverName, tt.verifyAs, err)
		}
	}
}

func TestCertIssuerCachesAndRenews(t *testing.T) {
	ci, _ := newTestIssuer(t, "yt")
	first, err := ci.certificate(&tls.ClientHelloInfo{ServerName: "yt"})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := ci.certificate(&tls.ClientHelloInfo{ServerName: "yt"})
	if again != first {
		t.Error("second handshake signed a new certificate instead of reusing it")
	}
	ci.now = func() time.Time { return time.Now().Add(leafLifetime - leafRenewal/2) }
	renewed, _ := ci.certificate(&tls.ClientHelloInfo{ServerName: "yt"})
	if renewed == first {
		t.Error("certificate close to expiry was not renewed")
	}
}

func TestHTTPSRedirect(t *testing.T) {
	s, _ := newTestServer(t) // yt -> https://youtube.com
	ci, pool := newTestIssuer(t)
	ci.allowed = s.servesName
	ts := httptest.NewUnstartedServer(s)
	ts.TLS = ci.tlsConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	hc := pinnedClient(ts.Listener.Addr().String(), pool)
	resp, err := hc.Get("https://yt/watch?v=1")
	if err != nil {
		t.Fatalf("https://yt: %v", err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || loc != "https://youtube.com/watch?v=1" {
		t.Errorf("https://yt/watch?v=1 = %d %q", resp.StatusCode, loc)
	}
	if _, err := hc.Get("https://google.com/"); err == nil {
		t.Error("hop served a certificate for google.com")
	}
}
