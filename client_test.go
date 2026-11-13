package main

import (
	"net/http/httptest"
	"os"
	"testing"
)

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in   string
		want Service
	}{
		{"8080", Service{Port: 8080}},
		{":8080", Service{Port: 8080}},
		{"localhost:8080", Service{Port: 8080}},
		{"LOCALHOST:8080/", Service{Port: 8080}},
		{"127.0.0.1:8080", Service{Port: 8080}},
		{"http://localhost:8080", Service{Port: 8080}},
		{"[::1]:8080", Service{Port: 8080}},
		{"99999999999999999999", Service{Port: -1}},
		// A path, or https, means a website that happens to be local.
		{"http://localhost:8080/admin", Service{URL: "http://localhost:8080/admin"}},
		{"https://localhost:8443", Service{URL: "https://localhost:8443"}},
		{"localhost8080", Service{URL: "https://localhost8080"}},
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

func newTestClient(t *testing.T, s *Server) client {
	t.Helper()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	return client{base: ts.URL, http: ts.Client()}
}

func TestClientErrors(t *testing.T) {
	s, _ := newTestServer(t)
	c := newTestClient(t, s)
	if _, err := c.put(Service{Name: "yt", URL: "https://github.com"}, false); !isNameTaken(err) {
		t.Errorf("put over a taken name: %v, want name taken", err)
	}
	if _, err := c.put(Service{Name: "yt", URL: "https://github.com"}, true); err != nil {
		t.Errorf("put with replace: %v", err)
	}
	if err := c.remove("nope", 0); !isNotFound(err) {
		t.Errorf("remove of a missing name: %v, want not found", err)
	}
}

func TestClaimMode(t *testing.T) {
	s, _ := newTestServer(t) // yt is a permanent shortcut
	mustPut(t, s.reg, Service{Name: "web", Port: 5173, PID: os.Getpid()})
	c := newTestClient(t, s)
	tests := []struct {
		name        string
		force       bool
		wantReplace bool
		wantErr     bool
	}{
		{"fresh", false, false, false},
		{"web", false, true, false}, // an earlier run of the app: take it over
		{"yt", false, false, true},  // a permanent shortcut needs -f
		{"yt", true, true, false},
	}
	for _, tt := range tests {
		replace, err := claimMode(c, tt.name, tt.force)
		if replace != tt.wantReplace || (err != nil) != tt.wantErr {
			t.Errorf("claimMode(%q, force=%v) = %v, %v; want %v, wantErr %v", tt.name, tt.force, replace, err, tt.wantReplace, tt.wantErr)
		}
	}
	down := client{base: "http://127.0.0.1:1", http: c.http}
	if replace, err := claimMode(down, "web", false); replace || err != nil {
		t.Errorf("claimMode with no server = %v, %v; want false, nil so the app still runs", replace, err)
	}
}
