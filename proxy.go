package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A local app gets each request as if the browser had opened
// localhost:<port> itself. Dev servers (Vite, webpack, Next, Rails) check
// the Host and Origin headers against localhost and refuse anything else,
// so hop rewrites both, and points absolute redirects back at the name.

// appTarget is the local app a request is for, and how the browser reached it.
type appTarget struct {
	name   string
	port   int
	origin string // scheme://host the browser used, e.g. http://web
}

type appTargetKey struct{}

func withAppTarget(r *http.Request, t appTarget) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), appTargetKey{}, t))
}

func appTargetOf(r *http.Request) appTarget {
	return r.Context().Value(appTargetKey{}).(appTarget)
}

func newAppProxy(errorHandler func(http.ResponseWriter, *http.Request, error)) *httputil.ReverseProxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialLoopback
	return &httputil.ReverseProxy{
		Transport:      transport,
		Rewrite:        rewriteAppRequest,
		ModifyResponse: rewriteAppResponse,
		ErrorHandler:   errorHandler,
	}
}

func rewriteAppRequest(pr *httputil.ProxyRequest) {
	t := appTargetOf(pr.In)
	local := "localhost:" + strconv.Itoa(t.port)
	pr.SetURL(&url.URL{Scheme: "http", Host: local})
	pr.SetXForwarded()
	// Frameworks that trust X-Forwarded-Host (Rails, Rack) would check it
	// against their allowed hosts instead of the localhost Host header.
	pr.Out.Header.Del("X-Forwarded-Host")
	pr.Out.Host = local
	// Only the app's own pages get a localhost Origin; a request from any
	// other site keeps its Origin, so the app's CSRF checks still see it.
	if strings.EqualFold(pr.In.Header.Get("Origin"), t.origin) {
		pr.Out.Header.Set("Origin", "http://"+local)
	}
}

// rewriteAppResponse points absolute redirects to localhost:<port> back at
// the name, so the browser stays on it and keeps the cookies set there.
func rewriteAppResponse(resp *http.Response) error {
	if loc := resp.Header.Get("Location"); loc != "" {
		resp.Header.Set("Location", publicLocation(loc, appTargetOf(resp.Request)))
	}
	return nil
}

func publicLocation(loc string, t appTarget) string {
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		return loc
	}
	port := ":" + strconv.Itoa(t.port)
	switch strings.ToLower(u.Host) {
	case "localhost" + port, "127.0.0.1" + port, "[::1]" + port:
	default:
		return loc
	}
	public, err := url.Parse(t.origin)
	if err != nil {
		return loc
	}
	u.Scheme, u.Host = public.Scheme, public.Host
	return u.String()
}

// dialLoopback connects to the port in addr on 127.0.0.1, then on ::1. Apps
// that listen on "localhost" often bind only one of the two: Vite on recent
// Node versions picks ::1 on macOS. The host in addr is ignored, so the
// proxy can only ever reach this machine.
func dialLoopback(ctx context.Context, _, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err4 := d.DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", port))
	if err4 == nil {
		return conn, nil
	}
	conn, err6 := d.DialContext(ctx, "tcp6", net.JoinHostPort("::1", port))
	if err6 == nil {
		return conn, nil
	}
	return nil, errors.Join(err4, err6)
}

// portStatus reports whether anything answers on a local port.
func portStatus(port int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	conn, err := dialLoopback(ctx, "tcp", net.JoinHostPort("localhost", strconv.Itoa(port)))
	if err != nil {
		return "down"
	}
	conn.Close()
	return "up"
}
