package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

// Host names that mean hop itself: the dashboard, the API and go/<name>
// links. Loopback IPs count too; see isHopHost.
var hopHosts = []string{"go", "hop", "localhost", "go.localhost", "hop.localhost"}

// Server routes by Host header: website names redirect, local app names are
// reverse proxied to their port, and hop's own names serve the dashboard,
// the API and go/<name> links.
type Server struct {
	reg   *Registry
	links linkStyle
	proxy *httputil.ReverseProxy
	api   http.Handler
}

// linkStyle builds the URL a person types for a name on this machine.
type linkStyle struct {
	shortNames bool // the hosts file is managed, so yt/ resolves, not just yt.localhost
	httpPort   int
	httpsPort  int // 0 when HTTPS is off
}

func (l linkStyle) url(scheme, name string) string {
	host, port, defaultPort := name, l.httpPort, 80
	if scheme == "https" {
		port, defaultPort = l.httpsPort, 443
	}
	if !l.shortNames {
		host += ".localhost"
	}
	if port != defaultPort {
		host += ":" + strconv.Itoa(port)
	}
	return scheme + "://" + host
}

func newServer(reg *Registry, links linkStyle) *Server {
	s := &Server{reg: reg, links: links}
	s.proxy = newAppProxy(s.writeAppNotResponding)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_hop/{$}", serveDashboard)
	mux.HandleFunc("GET /_hop/api/services", s.listServices)
	mux.HandleFunc("POST /_hop/api/services", s.putService)
	mux.HandleFunc("DELETE /_hop/api/services/{name}", s.deleteService)
	mux.HandleFunc("GET /_hop/api/free-port", serveFreePort)
	s.api = sameMachineOnly(mux)
	return s
}

// Only hop's own hosts serve /_hop/, so a proxied app keeps every path of
// its own, including /_hop/.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	if !isHopHost(host) {
		s.routeByHost(w, r, strings.TrimSuffix(host, ".localhost"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_hop/") {
		s.api.ServeHTTP(w, r)
		return
	}
	name, rest := splitFirstSegment(r.URL.EscapedPath())
	if name == "" {
		http.Redirect(w, r, "/_hop/", http.StatusFound)
		return
	}
	// Browsers lowercase host names but not paths, so go/YT must find yt.
	s.routeFromGo(w, r, strings.ToLower(name), rest)
}

// routeFromGo handles go/<name>/<rest>. Local apps are sent to their own host
// rather than proxied under a path prefix, which would break absolute asset URLs.
func (s *Server) routeFromGo(w http.ResponseWriter, r *http.Request, name, rest string) {
	svc, ok := s.reg.Lookup(name)
	if !ok {
		s.writeUnknownName(w, r, name)
		return
	}
	if svc.isLocal() {
		http.Redirect(w, r, s.links.url(requestScheme(r), name)+withQuery(rest, r.URL.RawQuery), http.StatusFound)
		return
	}
	s.reg.RecordHit(name)
	http.Redirect(w, r, redirectTarget(svc.URL, rest, r.URL), http.StatusFound)
}

func (s *Server) routeByHost(w http.ResponseWriter, r *http.Request, name string) {
	svc, ok := s.reg.Lookup(name)
	if !ok {
		s.writeUnknownName(w, r, name)
		return
	}
	if !svc.isLocal() {
		s.reg.RecordHit(name)
		http.Redirect(w, r, redirectTarget(svc.URL, r.URL.EscapedPath(), r.URL), http.StatusFound)
		return
	}
	// Count page opens, not every asset and API call the page makes.
	if r.Header.Get("Sec-Fetch-Mode") == "navigate" {
		s.reg.RecordHit(name)
	}
	origin := requestScheme(r) + "://" + r.Host
	s.proxy.ServeHTTP(w, withAppTarget(r, appTarget{name: name, port: svc.Port, origin: origin}))
}

func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func requestHost(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// splitFirstSegment turns "/yt/watch" into ("yt", "/watch").
func splitFirstSegment(path string) (string, string) {
	path = strings.TrimPrefix(path, "/")
	name, rest, found := strings.Cut(path, "/")
	if !found {
		return name, ""
	}
	return name, "/" + rest
}

// sameMachineOnly blocks web pages from changing the registry: a malicious
// site could otherwise POST here from the browser. Requiring JSON forces a
// CORS preflight, which hop never approves. DNS rebinding cannot reach the
// API because ServeHTTP only routes hop's own names to it.
func sameMachineOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !isLocalHostName(strings.ToLower(u.Hostname())) {
				http.Error(w, "cross-site request blocked", http.StatusForbidden)
				return
			}
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "send JSON with Content-Type: application/json", http.StatusUnsupportedMediaType)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// servesName reports whether hop answers for name, which is what the HTTPS
// certificate issuer may sign.
func (s *Server) servesName(name string) bool {
	if isHopHost(name) {
		return true
	}
	_, ok := s.reg.Lookup(name)
	return ok
}

// isHopHost reports whether host means hop itself: one of its names, or a
// loopback IP such as 127.0.0.2, which is how the CLI reaches it.
func isHopHost(host string) bool {
	if slices.Contains(hopHosts, host) {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// Single-word names like yt can only come from the hosts file, never from
// public DNS, so they are as trustworthy as localhost.
func isLocalHostName(host string) bool {
	return isHopHost(host) || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".")
}

func serveDashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// No framing, so another page cannot trick a click on Remove or Replace.
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	if _, err := w.Write(dashboardHTML); err != nil {
		slog.Debug("write dashboard failed", "err", err)
	}
}

// serviceView is a Service plus what the dashboard and CLI need to show it.
type serviceView struct {
	Service
	Link   string `json:"link"`
	Status string `json:"status"` // "up", "down" or "redirect"
}

type listResponse struct {
	Services []serviceView `json:"services"`
}

func (s *Server) view(svc Service) serviceView {
	return serviceView{Service: svc, Link: s.links.url("http", svc.Name) + "/", Status: "redirect"}
}

func (s *Server) listServices(w http.ResponseWriter, _ *http.Request) {
	list := s.reg.List()
	views := make([]serviceView, len(list))
	var wg sync.WaitGroup
	for i, svc := range list {
		views[i] = s.view(svc)
		if svc.isLocal() {
			wg.Go(func() { views[i].Status = portStatus(svc.Port) })
		}
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, listResponse{Services: views})
}

// putRequest is a Service plus whether it may replace an existing name.
// Replace defaults to true, because apps re-register their name on every
// restart; the CLI, dashboard and MCP send false so people do not overwrite
// a shortcut by accident.
type putRequest struct {
	Service
	Replace *bool `json:"replace,omitempty"`
}

func (s *Server) putService(w http.ResponseWriter, r *http.Request) {
	var in putRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, `body must be JSON like {"name":"yt","url":"https://youtube.com"}`)
		return
	}
	svc := Service{Name: strings.ToLower(strings.TrimSpace(in.Name)), URL: strings.TrimSpace(in.URL), Port: in.Port, PID: in.PID}
	replace := in.Replace == nil || *in.Replace
	svc, err := s.reg.Put(svc, replace)
	var taken *NameTakenError
	switch {
	case errors.As(err, &taken):
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Info("service registered", "name", svc.Name, "url", svc.URL, "port", svc.Port, "pid", svc.PID)
	view := s.view(svc)
	if svc.isLocal() {
		view.Status = portStatus(svc.Port)
	}
	writeJSON(w, http.StatusCreated, view)
}

// deleteService removes a name. With ?pid=N it removes the name only while
// that process holds it, which is how hop run cleans up after itself.
func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(r.PathValue("name"))
	pid := 0
	if p := r.URL.Query().Get("pid"); p != "" {
		var err error
		if pid, err = strconv.Atoi(p); err != nil || pid <= 0 {
			writeJSONError(w, http.StatusBadRequest, "pid must be a positive number")
			return
		}
	}
	if !s.reg.Remove(name, pid) {
		msg := fmt.Sprintf("no service named %q", name)
		if pid > 0 {
			msg += fmt.Sprintf(" registered by process %d", pid)
		}
		writeJSONError(w, http.StatusNotFound, msg)
		return
	}
	slog.Info("service removed", "name", name)
	w.WriteHeader(http.StatusNoContent)
}

func serveFreePort(w http.ResponseWriter, _ *http.Request) {
	port, err := freePort()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"port": port})
}

// freePort asks the kernel for an unused port. Another process could take it
// before the app binds, but on a dev machine that race is rare enough.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("write response failed", "err", err)
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// dashboardLink is absolute because these pages are shown on other hosts,
// where /_hop/ belongs to the app, not to hop.
func (s *Server) dashboardLink(r *http.Request) string {
	return s.links.url(requestScheme(r), "hop") + "/_hop/"
}

func (s *Server) writeUnknownName(w http.ResponseWriter, r *http.Request, name string) {
	n := html.EscapeString(name)
	body := `Nothing is registered as <b>` + n + `</b>.`
	if validateName(name) == nil {
		add := html.EscapeString(s.dashboardLink(r) + "?add=" + url.QueryEscape(name))
		body += ` <a href="` + add + `">Add it on the dashboard</a>.`
	}
	writeMessagePage(w, http.StatusNotFound, "No shortcut named "+n, body)
}

func (s *Server) writeAppNotResponding(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return // the browser left; nobody is there to read a page
	}
	t := appTargetOf(r)
	slog.Debug("proxy failed", "name", t.name, "port", t.port, "err", err)
	n := html.EscapeString(t.name)
	writeMessagePage(w, http.StatusBadGateway, n+" is not responding",
		fmt.Sprintf(`<b>%s</b> is registered on port %d, but nothing is answering there. Start the app, or <a href="%s">remove it on the dashboard</a>.`,
			n, t.port, html.EscapeString(s.dashboardLink(r))))
}

func writeMessagePage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:15vh auto;padding:0 16px;color:#1f2328;background:#fff}
@media(prefers-color-scheme:dark){body{color:#e6e8eb;background:#111316}a{color:#7cb3ff}}h1{font-size:20px;font-weight:600}</style>
<h1>%s</h1><p>%s</p>`, title, title, body)
}

// maintain persists the registry, drops names of apps that exited and keeps
// the hosts file in step with the names, until ctx is done.
func (s *Server) maintain(ctx context.Context, hostsPath, ip string) {
	hosts := &hostsSyncer{path: hostsPath, ip: ip}
	hosts.sync(s.reg.Names())
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, name := range s.reg.ReapDead() {
				slog.Info("app exited, name removed", "name", name)
			}
			s.reg.SaveIfDirty()
			// Also retries a failed sync, and restores hop's block if
			// something else rewrote the file.
			hosts.sync(s.reg.Names())
		case <-s.reg.changed:
			s.reg.SaveIfDirty()
			hosts.sync(s.reg.Names())
		}
	}
}
