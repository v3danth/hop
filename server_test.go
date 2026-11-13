package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
		{"go is case-insensitive", "go", "/YT", http.StatusFound, "https://youtube.com", ""},
		{"local app via go redirects to its host", "go", "/app/x?y=1", http.StatusFound, "http://app/x?y=1", ""},
		{"https go keeps https", "go", "https://go/app/x", http.StatusFound, "https://app/x", ""},
		{"local app proxied", "app.localhost", "/hello", http.StatusOK, "", "app:/hello"},
		{"app keeps its own /_hop/ paths", "app", "/_hop/api/services", http.StatusOK, "", "app:/_hop/api/services"},
		{"dashboard root redirect", "hop", "/", http.StatusFound, "/_hop/", ""},
		{"api on localhost", "127.0.0.1", "/_hop/api/services", http.StatusOK, "", `"name":"app"`},
		{"api on hop's own address", "127.0.0.2", "/_hop/api/services", http.StatusOK, "", `"name":"app"`},
		{"api on ipv6 loopback", "[::1]", "/_hop/api/services", http.StatusOK, "", `"name":"app"`},
		{"unknown name links to dashboard", "nope", "/", http.StatusNotFound, "", `href="http://hop/_hop/?add=nope"`},
		{"unknown name over https links to https dashboard", "nope", "https://nope/", http.StatusNotFound, "", `href="https://hop/_hop/?add=nope"`},
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

func TestUnknownNameOffersAddOnlyForValidNames(t *testing.T) {
	s, _ := newTestServer(t)
	if body := serve(s, "some.example", "/").Body.String(); strings.Contains(body, "Add it") {
		t.Errorf("offered to add an invalid name: %q", body)
	}
}

func TestLinkStyle(t *testing.T) {
	tests := []struct {
		links  linkStyle
		scheme string
		want   string
	}{
		{linkStyle{shortNames: true, httpPort: 80, httpsPort: 443}, "http", "http://yt"},
		{linkStyle{shortNames: true, httpPort: 80, httpsPort: 443}, "https", "https://yt"},
		{linkStyle{shortNames: false, httpPort: 8080, httpsPort: 8443}, "http", "http://yt.localhost:8080"},
		{linkStyle{shortNames: false, httpPort: 8080, httpsPort: 8443}, "https", "https://yt.localhost:8443"},
	}
	for _, tt := range tests {
		if got := tt.links.url(tt.scheme, "yt"); got != tt.want {
			t.Errorf("%+v.url(%s) = %q, want %q", tt.links, tt.scheme, got, tt.want)
		}
	}
}

func TestAppNotRespondingLinksToDashboard(t *testing.T) {
	s, _ := newTestServer(t)
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, s.reg, Service{Name: "dead", Port: port})
	rec := serve(s, "dead", "/")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `href="http://hop/_hop/"`) {
		t.Errorf("body %q has no absolute dashboard link", rec.Body)
	}
}

func TestDashboardCannotBeFramed(t *testing.T) {
	s, _ := newTestServer(t)
	rec := serve(s, "hop", "/_hop/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if xfo := rec.Header().Get("X-Frame-Options"); xfo != "DENY" {
		t.Errorf("X-Frame-Options = %q", xfo)
	}
}

func post(s *Server, origin, contentType, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/_hop/api/services", strings.NewReader(body))
	req.Host = "hop"
	req.Header.Set("Content-Type", contentType)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestAPIRejectsCrossSiteWrites(t *testing.T) {
	s, _ := newTestServer(t)
	body := `{"name":"x","port":1234}`
	if rec := serve(s, "evil.example", "/_hop/api/services"); rec.Code != http.StatusNotFound {
		t.Errorf("public host reached the api: %d", rec.Code)
	}
	if rec := post(s, "https://evil.example", "application/json", body); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site origin: %d, want 403", rec.Code)
	}
	if rec := post(s, "", "text/plain", body); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form post: %d, want 415", rec.Code)
	}
	if rec := post(s, "http://hop", "application/json", body); rec.Code != http.StatusCreated {
		t.Errorf("local json: %d, want 201; %q", rec.Code, rec.Body)
	}
}

func TestPutServiceReplaceFlag(t *testing.T) {
	s, _ := newTestServer(t) // yt -> https://youtube.com
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
			if rec := post(s, "", "application/json", tt.body); rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if svc, _ := s.reg.Lookup("yt"); svc.URL != tt.wantURL {
				t.Errorf("yt goes to %s, want %s", svc.URL, tt.wantURL)
			}
		})
	}
}

func TestDeleteService(t *testing.T) {
	s, _ := newTestServer(t)
	mustPut(t, s.reg, Service{Name: "web", Port: 5173, PID: 200})
	del := func(target string) int {
		req := httptest.NewRequest(http.MethodDelete, target, nil)
		req.Host = "hop"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code
	}
	tests := []struct {
		target string
		want   int
	}{
		{"/_hop/api/services/web?pid=abc", http.StatusBadRequest},
		{"/_hop/api/services/web?pid=100", http.StatusNotFound}, // held by pid 200
		{"/_hop/api/services/WEB?pid=200", http.StatusNoContent},
		{"/_hop/api/services/web", http.StatusNotFound},
		{"/_hop/api/services/YT", http.StatusNoContent},
	}
	for _, tt := range tests {
		if got := del(tt.target); got != tt.want {
			t.Errorf("DELETE %s = %d, want %d", tt.target, got, tt.want)
		}
	}
}
