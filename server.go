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

// Hosts that mean "hop itself": the dashboard, or go/<name> path routing.
var hopHosts = []string{"go", "hop", "localhost", "127.0.0.1", "::1", "go.localhost", "hop.localhost"}

// Server routes by Host header: website names redirect, local app names are
// reverse proxied to their loopback port.
type Server struct {
	reg        *Registry
	listenPort int
	shortNames bool // hosts file is managed, so single-word names resolve
	proxy      *httputil.ReverseProxy
	api        http.Handler
}

type proxyTarget struct {
	name string
	port int
}

type proxyTargetKey struct{}

func newServer(reg *Registry, listenPort int, shortNames bool) *Server {
	s := &Server{reg: reg, listenPort: listenPort, shortNames: shortNames}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(proxyTargetKey{}).(proxyTarget)
			pr.SetURL(&url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(t.port)})
			pr.SetXForwarded()
			// Dev servers (Vite, webpack, Next) reject unknown Host headers, so
			// the app sees the request as if it came to localhost directly.
			pr.Out.Host = "localhost:" + strconv.Itoa(t.port)
		},
		ErrorHandler: s.writeAppNotResponding,
	}

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
	if isHopHost(host) {
		if strings.HasPrefix(r.URL.Path, "/_hop/") {
			s.api.ServeHTTP(w, r)
			return
		}
		name, rest := splitFirstSegment(r.URL.EscapedPath())
		if name == "" {
			http.Redirect(w, r, "/_hop/", http.StatusFound)
			return
		}
		s.routeFromGo(w, r, name, rest)
		return
	}
	s.routeByHost(w, r, strings.TrimSuffix(host, ".localhost"))
}

// routeFromGo handles go/<name>/<rest>. Local apps are sent to their own host
// rather than proxied under a path prefix, which would break absolute asset URLs.
func (s *Server) routeFromGo(w http.ResponseWriter, r *http.Request, name, rest string) {
	svc, ok := s.reg.Lookup(name)
	if !ok {
		s.writeUnknownName(w, name)
		return
	}
	if svc.isLocal() {
		http.Redirect(w, r, s.serviceLink(name)+withQuery(rest, r.URL.RawQuery), http.StatusFound)
		return
	}
	s.reg.RecordHit(name)
	http.Redirect(w, r, redirectTarget(svc.URL, rest, r.URL), http.StatusFound)
}

func (s *Server) routeByHost(w http.ResponseWriter, r *http.Request, name string) {
	svc, ok := s.reg.Lookup(name)
	if !ok {
		s.writeUnknownName(w, name)
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
	ctx := context.WithValue(r.Context(), proxyTargetKey{}, proxyTarget{name: name, port: svc.Port})
	s.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// serviceLink is the URL a person types for name, matching how names resolve
// on this machine.
func (s *Server) serviceLink(name string) string {
	host := name
	if !s.shortNames {
		host += ".localhost"
	}
	if s.listenPort != 80 {
		host += ":" + strconv.Itoa(s.listenPort)
	}
	return "http://" + host
}

// redirectTarget appends the rest of the path to a website shortcut, or fills
// it into %s so a shortcut like yt -> youtube.com/results?search_query=%s
// turns yt/cats into a search. rest is the escaped path after the name.
func redirectTarget(base, rest string, u *url.URL) string {
	if strings.Contains(base, "%s") {
		return strings.ReplaceAll(base, "%s", url.QueryEscape(searchTerm(rest, u)))
	}
	if rest == "/" {
		rest = ""
	}
	return withQuery(strings.TrimRight(base, "/")+rest, u.RawQuery)
}

// searchTerm rebuilds what the person typed after name/. The browser splits
// a term like "what is 2+2?" at the "?", so the query is part of the term.
func searchTerm(rest string, u *url.URL) string {
	raw := strings.TrimPrefix(rest, "/")
	if u.RawQuery != "" || u.ForceQuery {
		raw += "?" + u.RawQuery
	}
	term, err := url.PathUnescape(raw)
	if err != nil {
		return raw
	}
	return term
}

func withQuery(u, query string) string {
	if query == "" {
		return u
	}
	return u + "?" + query
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
// site could otherwise POST here from the browser. DNS rebinding cannot reach
// the API because ServeHTTP only routes hop's own local host names to it.
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

// isHopHost reports whether host means hop itself: its names, or a loopback
// IP such as 127.0.0.2 that the CLI uses to reach it.
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
	w.Write(dashboardHTML)
}

// serviceView is a Service plus what the dashboard and CLI need to show it.
type serviceView struct {
	Service
	Link   string `json:"link"`
	Status string `json:"status"` // "up", "down" or "redirect"
}

type listResponse struct {
	ListenPort int           `json:"listenPort"`
	ShortNames bool          `json:"shortNames"`
	Services   []serviceView `json:"services"`
}

func (s *Server) listServices(w http.ResponseWriter, _ *http.Request) {
	list := s.reg.List()
	views := make([]serviceView, len(list))
	var wg sync.WaitGroup
	for i, svc := range list {
		views[i] = serviceView{Service: svc, Link: s.serviceLink(svc.Name) + "/", Status: "redirect"}
		if svc.isLocal() {
			wg.Go(func() { views[i].Status = portStatus(svc.Port) })
		}
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, listResponse{ListenPort: s.listenPort, ShortNames: s.shortNames, Services: views})
}

func portStatus(port int) string {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
	if err != nil {
		return "down"
	}
	conn.Close()
	return "up"
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
		writeJSONError(w, http.StatusBadRequest, "body must be JSON like {\"name\":\"yt\",\"url\":\"https://youtube.com\"}")
		return
	}
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	in.URL = strings.TrimSpace(in.URL)
	put := s.reg.Put
	if in.Replace != nil && !*in.Replace {
		put = s.reg.Add
	}
	svc, err := put(Service{Name: in.Name, URL: in.URL, Port: in.Port, PID: in.PID})
	var taken *NameTakenError
	if errors.As(err, &taken) {
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Info("service registered", "name", svc.Name, "url", svc.URL, "port", svc.Port, "pid", svc.PID)
	status := "redirect"
	if svc.isLocal() {
		status = portStatus(svc.Port)
	}
	writeJSON(w, http.StatusCreated, serviceView{Service: svc, Link: s.serviceLink(svc.Name) + "/", Status: status})
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.reg.Remove(name) {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no service named %q", name))
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
	json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// dashboardLink is absolute because these pages are shown on other hosts,
// where /_hop/ belongs to the app, not to hop.
func (s *Server) dashboardLink() string {
	return s.serviceLink("hop") + "/_hop/"
}

func (s *Server) writeUnknownName(w http.ResponseWriter, name string) {
	n := html.EscapeString(name)
	add := html.EscapeString(s.dashboardLink() + "?add=" + url.QueryEscape(name))
	writeMessagePage(w, http.StatusNotFound, "No shortcut named "+n,
		`Nothing is registered as <b>`+n+`</b>. <a href="`+add+`">Add it on the dashboard</a>.`)
}

func (s *Server) writeAppNotResponding(w http.ResponseWriter, r *http.Request, err error) {
	t := r.Context().Value(proxyTargetKey{}).(proxyTarget)
	slog.Debug("proxy failed", "name", t.name, "port", t.port, "err", err)
	n := html.EscapeString(t.name)
	writeMessagePage(w, http.StatusBadGateway, n+" is not responding",
		fmt.Sprintf(`<b>%s</b> is registered on port %d, but nothing is answering there. Start the app, or <a href="%s">remove it on the dashboard</a>.`,
			n, t.port, html.EscapeString(s.dashboardLink())))
}

func writeMessagePage(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:15vh auto;padding:0 16px;color:#1f2328;background:#fff}
@media(prefers-color-scheme:dark){body{color:#e6e8eb;background:#111316}a{color:#7cb3ff}}h1{font-size:20px;font-weight:600}</style>
<h1>%s</h1><p>%s</p>`, title, title, body)
}

// maintain persists the registry, reaps exited apps and keeps the hosts file
// in step with the names.
func (s *Server) maintain(ctx context.Context, hostsPath, ip string) {
	syncHostsLogged(hostsPath, ip, s.reg.Names())
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
		case <-s.reg.changed:
			s.reg.SaveIfDirty()
			syncHostsLogged(hostsPath, ip, s.reg.Names())
		}
	}
}

func syncHostsLogged(path, ip string, names []string) {
	if path == "" {
		return
	}
	if err := syncHosts(path, ip, names); err != nil {
		slog.Error("hosts sync failed", "err", err)
	}
}
