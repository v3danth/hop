package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// listenIPv6Loopback starts an app that listens on ::1 only, the way Vite
// does on recent Node versions on macOS.
func listenIPv6Loopback(t *testing.T, h http.Handler) int {
	t.Helper()
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	app := httptest.NewUnstartedServer(h)
	app.Listener.Close()
	app.Listener = ln
	app.Start()
	t.Cleanup(app.Close)
	return portOf(t, ln.Addr())
}

func TestProxyReachesAppListeningOnIPv6Only(t *testing.T) {
	port := listenIPv6Loopback(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "v6 app")
	}))
	s, _ := newTestServer(t)
	mustPut(t, s.reg, Service{Name: "v6", Port: port})
	if rec := serve(s, "v6", "/"); rec.Code != http.StatusOK || rec.Body.String() != "v6 app" {
		t.Errorf("v6/ = %d %q, want 200 from the app", rec.Code, rec.Body)
	}
	if got := portStatus(port); got != "up" {
		t.Errorf("portStatus = %q, want up", got)
	}
}

func TestProxyPresentsRequestsAsLocalhost(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := json.NewEncoder(w).Encode(map[string]string{
			"host":          r.Host,
			"origin":        r.Header.Get("Origin"),
			"forwardedHost": r.Header.Get("X-Forwarded-Host"),
			"forwardedFor":  r.Header.Get("X-Forwarded-For"),
		})
		if err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(app.Close)
	port := portOf(t, app.Listener.Addr())
	local := "localhost:" + strconv.Itoa(port)
	s, _ := newTestServer(t)
	mustPut(t, s.reg, Service{Name: "web", Port: port})

	tests := []struct {
		name, origin, wantOrigin string
	}{
		{"no origin", "", ""},
		// Vite and Next refuse WebSocket and API requests from origins
		// other than localhost, so the app's own pages get a localhost one.
		{"the app's own page", "http://web", "http://" + local},
		// Another site keeps its origin, so the app can still refuse it.
		{"another site", "https://evil.example", "https://evil.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Host = "web"
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			var got map[string]string
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode app echo: %v (status %d)", err, rec.Code)
			}
			if got["host"] != local {
				t.Errorf("app saw Host %q, want %q", got["host"], local)
			}
			if got["origin"] != tt.wantOrigin {
				t.Errorf("app saw Origin %q, want %q", got["origin"], tt.wantOrigin)
			}
			if got["forwardedHost"] != "" {
				t.Errorf("app saw X-Forwarded-Host %q, want none", got["forwardedHost"])
			}
			if got["forwardedFor"] == "" {
				t.Error("app saw no X-Forwarded-For")
			}
		})
	}
}

func TestProxyPointsRedirectsBackAtTheName(t *testing.T) {
	var local string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/absolute":
			http.Redirect(w, r, "http://"+local+"/login?next=%2F", http.StatusFound)
		case "/elsewhere":
			http.Redirect(w, r, "https://accounts.example/auth", http.StatusFound)
		case "/relative":
			http.Redirect(w, r, "/login", http.StatusFound)
		}
	}))
	t.Cleanup(app.Close)
	port := portOf(t, app.Listener.Addr())
	local = "localhost:" + strconv.Itoa(port)
	s, _ := newTestServer(t)
	mustPut(t, s.reg, Service{Name: "web", Port: port})

	tests := []struct {
		target, want string
	}{
		{"http://web/absolute", "http://web/login?next=%2F"},
		{"https://web/absolute", "https://web/login?next=%2F"},
		{"http://web/elsewhere", "https://accounts.example/auth"},
		{"http://web/relative", "/login"},
	}
	for _, tt := range tests {
		rec := serve(s, "web", tt.target)
		if loc := rec.Header().Get("Location"); loc != tt.want {
			t.Errorf("%s redirected to %q, want %q", tt.target, loc, tt.want)
		}
	}
}

func TestPortStatus(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(t, ln.Addr())
	if got := portStatus(port); got != "up" {
		t.Errorf("listening port: %q, want up", got)
	}
	ln.Close()
	if got := portStatus(port); got != "down" {
		t.Errorf("closed port: %q, want down", got)
	}
}
