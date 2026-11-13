package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHostsWithNames(t *testing.T) {
	tests := []struct {
		name, current string
		names         []string
		want          string
	}{
		{
			name:    "adds block",
			current: "127.0.0.1 localhost\n",
			names:   []string{"yt"},
			want:    "127.0.0.1 localhost\n\n" + hostsBegin + "\n127.0.0.1 yt yt.localhost\n" + hostsEnd + "\n",
		},
		{
			name:    "replaces block and keeps lines after it",
			current: "a\n\n" + hostsBegin + "\n127.0.0.1 old old.localhost\n" + hostsEnd + "\nb\n",
			names:   []string{"new"},
			want:    "a\n\nb\n\n" + hostsBegin + "\n127.0.0.1 new new.localhost\n" + hostsEnd + "\n",
		},
		{
			name:    "empty file",
			current: "",
			names:   nil,
			want:    hostsBegin + "\n" + hostsEnd + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostsWithNames(tt.current, "127.0.0.1", tt.names); got != tt.want {
				t.Errorf("hostsWithNames() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestHostsWithNamesIsIdempotent(t *testing.T) {
	once := hostsWithNames("127.0.0.1 localhost\n", "127.0.0.2", []string{"a", "b"})
	if twice := hostsWithNames(once, "127.0.0.2", []string{"a", "b"}); twice != once {
		t.Errorf("second sync changed the file:\n%q\nwant\n%q", twice, once)
	}
}

func TestSyncHostsKeepsModeAndContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := syncHosts(path, "127.0.0.2", []string{"yt"}); err != nil {
		t.Fatalf("syncHosts: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "127.0.0.2 yt yt.localhost\n") || !strings.HasPrefix(string(b), "127.0.0.1 localhost\n") {
		t.Errorf("hosts file = %q", b)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", info.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".hosts.tmp-*")); len(leftovers) > 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestSyncHostsReplacesSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-hosts")
	link := filepath.Join(dir, "hosts")
	if err := os.WriteFile(real, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := syncHosts(link, "127.0.0.2", []string{"yt"}); err != nil {
		t.Fatalf("syncHosts: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink was replaced by a regular file")
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "registry.json")
	reg, err := loadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Put(Service{Name: "yt", URL: "https://youtube.com"}); err != nil {
		t.Fatal(err)
	}
	reg.SaveIfDirty()
	loaded, err := loadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if svc, ok := loaded.Lookup("yt"); !ok || svc.URL != "https://youtube.com" {
		t.Errorf("Lookup(yt) = %+v, %v", svc, ok)
	}
}

func TestUndoRemoveKeepsHistory(t *testing.T) {
	reg, err := loadRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Put(Service{Name: "yt", URL: "https://youtube.com"}); err != nil {
		t.Fatal(err)
	}
	reg.RecordHit("yt")
	reg.RecordHit("yt")
	reg.Remove("yt")
	restored, err := reg.Put(Service{Name: "yt", URL: "https://youtube.com"})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Hits != 2 || restored.LastHit.IsZero() {
		t.Errorf("restored = %+v, want 2 hits and a last visit", restored)
	}
	if other, _ := reg.Put(Service{Name: "gh", URL: "https://github.com"}); other.Hits != 0 {
		t.Errorf("unrelated name inherited history: %+v", other)
	}
}

func TestLoopbackIP(t *testing.T) {
	tests := []struct {
		host    string
		want    string
		wantErr bool
	}{
		{"127.0.0.2", "127.0.0.2", false},
		{"::1", "::1", false},
		{"0.0.0.0", "", true},
		{"192.168.1.5", "", true},
		{"localhost", "", true},
	}
	for _, tt := range tests {
		got, err := loopbackIP(tt.host)
		if got != tt.want || (err != nil) != tt.wantErr {
			t.Errorf("loopbackIP(%q) = %q, %v; want %q, wantErr %v", tt.host, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestAddKeepsExistingName(t *testing.T) {
	reg, err := loadRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Add(Service{Name: "yt", URL: "https://github.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Add(Service{Name: "yt", URL: "https://github.com"}); err != nil {
		t.Errorf("re-adding the same target: %v, want no error", err)
	}
	_, err = reg.Add(Service{Name: "yt", URL: "https://youtube.com"})
	var taken *NameTakenError
	if !errors.As(err, &taken) || err.Error() != "yt/ already goes to https://github.com" {
		t.Errorf("Add over a different target: %v, want NameTakenError", err)
	}
	if svc, _ := reg.Lookup("yt"); svc.URL != "https://github.com" {
		t.Errorf("yt now goes to %s, want it unchanged", svc.URL)
	}
}

func TestPutServiceReplaceFlag(t *testing.T) {
	s, _ := newTestServer(t) // yt -> https://youtube.com
	post := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/_hop/api/services", strings.NewReader(body))
		req.Host = "hop"
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code
	}
	tests := []struct {
		name, body string
		wantStatus int
		wantURL    string
	}{
		{"replace false keeps the name", `{"name":"yt","url":"https://github.com","replace":false}`, http.StatusConflict, "https://youtube.com"},
		{"same target is fine", `{"name":"yt","url":"https://youtube.com","replace":false}`, http.StatusCreated, "https://youtube.com"},
		{"apps without the flag still replace", `{"name":"yt","url":"https://github.com"}`, http.StatusCreated, "https://github.com"},
		{"replace true replaces", `{"name":"yt","url":"https://youtube.com","replace":true}`, http.StatusCreated, "https://youtube.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := post(tt.body); got != tt.wantStatus {
				t.Errorf("status = %d, want %d", got, tt.wantStatus)
			}
			if svc, _ := s.reg.Lookup("yt"); svc.URL != tt.wantURL {
				t.Errorf("yt goes to %s, want %s", svc.URL, tt.wantURL)
			}
		})
	}
}

func TestClientReportsNameTaken(t *testing.T) {
	s, _ := newTestServer(t)
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	c := client{base: ts.URL, http: ts.Client()}
	_, err := c.put(Service{Name: "yt", URL: "https://github.com"}, false)
	if !isNameTaken(err) {
		t.Fatalf("put without replace: %v, want a name-taken error", err)
	}
	if _, err := c.put(Service{Name: "yt", URL: "https://github.com"}, true); err != nil {
		t.Errorf("put with replace: %v", err)
	}
}

func TestValidateService(t *testing.T) {
	tests := []struct {
		svc     Service
		wantErr bool
	}{
		{Service{Name: "yt", URL: "https://youtube.com"}, false},
		{Service{Name: "ys", URL: "https://youtube.com/results?search_query=%s"}, false},
		{Service{Name: "api", Port: 8080}, false},
		{Service{Name: "Bad", Port: 8080}, true},
		{Service{Name: "-x", Port: 8080}, true},
		{Service{Name: "hop", Port: 8080}, true},
		{Service{Name: "x", Port: 70000}, true},
		{Service{Name: "x", URL: "ftp://a"}, true},
		{Service{Name: "x", URL: "https://a", Port: 1}, true},
		{Service{Name: "x"}, true},
	}
	for _, tt := range tests {
		if err := validateService(tt.svc); (err != nil) != tt.wantErr {
			t.Errorf("validateService(%+v) error = %v, wantErr %v", tt.svc, err, tt.wantErr)
		}
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in   string
		want Service
	}{
		{"8080", Service{Port: 8080}},
		{" youtube.com ", Service{URL: "https://youtube.com"}},
		{"http://a.test", Service{URL: "http://a.test"}},
		{"ftp://a.test", Service{URL: "ftp://a.test"}},
	}
	for _, tt := range tests {
		if got := parseTarget(tt.in); got != tt.want {
			t.Errorf("parseTarget(%q) = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestRedirectTarget(t *testing.T) {
	const search = "https://s.test/?q=%s"
	tests := []struct {
		base, target, want string
	}{
		{"https://a.test/", "/", "https://a.test"},
		{"https://a.test", "/x/y?z=1", "https://a.test/x/y?z=1"},
		{search, "/cats", "https://s.test/?q=cats"},
		{search, "/lofi%20beats", "https://s.test/?q=lofi+beats"},
		{search, "/100%25", "https://s.test/?q=100%25"},
		{search, "/what%20is%202+2?really", "https://s.test/?q=what+is+2%2B2%3Freally"},
		{search, "/why?", "https://s.test/?q=why%3F"},
	}
	for _, tt := range tests {
		u, err := url.Parse(tt.target)
		if err != nil {
			t.Fatal(err)
		}
		if got := redirectTarget(tt.base, u.EscapedPath(), u); got != tt.want {
			t.Errorf("redirectTarget(%q, %q) = %q, want %q", tt.base, tt.target, got, tt.want)
		}
	}
}

// newTestServer starts hop with one website shortcut and one local app that
// answers every path with "app:" plus the path.
func newTestServer(t *testing.T) (*Server, int) {
	t.Helper()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "app:"+r.URL.Path)
	}))
	t.Cleanup(app.Close)
	_, portText, _ := net.SplitHostPort(app.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	reg, err := loadRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range []Service{{Name: "yt", URL: "https://youtube.com"}, {Name: "app", Port: port}} {
		if _, err := reg.Put(svc); err != nil {
			t.Fatal(err)
		}
	}
	return newServer(reg, 80, true), port
}

func serve(s *Server, host, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestRouting(t *testing.T) {
	s, _ := newTestServer(t)
	tests := []struct {
		name, host, target string
		wantStatus         int
		wantLocation       string
		wantBody           string
	}{
		{"website by host", "yt", "/watch?v=1", http.StatusFound, "https://youtube.com/watch?v=1", ""},
		{"website by go", "go", "/yt/watch", http.StatusFound, "https://youtube.com/watch", ""},
		{"local app via go redirects to its host", "go", "/app/x?y=1", http.StatusFound, "http://app/x?y=1", ""},
		{"local app proxied", "app.localhost", "/hello", http.StatusOK, "", "app:/hello"},
		{"app keeps its own /_hop/ paths", "app", "/_hop/api/services", http.StatusOK, "", "app:/_hop/api/services"},
		{"dashboard root redirect", "hop", "/", http.StatusFound, "/_hop/", ""},
		{"api on hop host", "127.0.0.1", "/_hop/api/services", http.StatusOK, "", `"name":"app"`},
		{"api on hop's own address", "127.0.0.2", "/_hop/api/services", http.StatusOK, "", `"name":"app"`},
		{"api on ipv6 loopback", "[::1]", "/_hop/api/services", http.StatusOK, "", `"name":"app"`},
		{"unknown name links to dashboard", "nope", "/", http.StatusNotFound, "", `href="http://hop/_hop/?add=nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(s, tt.host, tt.target)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %q", rec.Code, tt.wantStatus, rec.Body)
			}
			if loc := rec.Header().Get("Location"); loc != tt.wantLocation {
				t.Errorf("Location = %q, want %q", loc, tt.wantLocation)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body %q does not contain %q", rec.Body, tt.wantBody)
			}
		})
	}
}

func TestAppNotRespondingLinksToDashboard(t *testing.T) {
	s, _ := newTestServer(t)
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.reg.Put(Service{Name: "dead", Port: port}); err != nil {
		t.Fatal(err)
	}
	rec := serve(s, "dead", "/")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `href="http://hop/_hop/"`) {
		t.Errorf("body %q has no absolute dashboard link", rec.Body)
	}
}

func TestAPIRejectsCrossSiteWrites(t *testing.T) {
	s, _ := newTestServer(t)
	tests := []struct {
		name, host, origin, contentType string
		wantStatus                      int
	}{
		{"public host never reaches the api", "evil.example", "", "application/json", http.StatusNotFound},
		{"cross-site origin", "hop", "https://evil.example", "application/json", http.StatusForbidden},
		{"form post", "hop", "", "text/plain", http.StatusUnsupportedMediaType},
		{"local json", "hop", "http://hop", "application/json", http.StatusCreated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/_hop/api/services", strings.NewReader(`{"name":"x","port":1234}`))
			req.Host = tt.host
			req.Header.Set("Content-Type", tt.contentType)
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body %q", rec.Code, tt.wantStatus, rec.Body)
			}
		})
	}
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
	ci, pool := newTestIssuer(t, "yt")
	ci.allowed = s.servesName
	ts := httptest.NewUnstartedServer(s)
	ts.TLS = ci.tlsConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)

	addr := ts.Listener.Addr().String()
	hc := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr) // what /etc/hosts does
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
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
