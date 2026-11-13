package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// client talks to a running hop server. The CLI and MCP server both use it, so
// the server stays the single owner of the registry file.
type client struct {
	base string
	http *http.Client
}

func newClient() client {
	base := os.Getenv("HOP_URL")
	if base == "" {
		base = "http://" + defaultIP
	}
	return client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 5 * time.Second}}
}

func (c client) list() (listResponse, error) {
	var out listResponse
	err := c.call(http.MethodGet, "/_hop/api/services", nil, &out)
	return out, err
}

// put registers s. With replace false, the server refuses a name that
// already goes somewhere else, and isNameTaken reports that.
func (c client) put(s Service, replace bool) (serviceView, error) {
	var out serviceView
	err := c.call(http.MethodPost, "/_hop/api/services", putRequest{Service: s, Replace: &replace}, &out)
	return out, err
}

// remove deletes name. With pid > 0 it does so only while that process
// holds the name; otherwise the server answers not found.
func (c client) remove(name string, pid int) error {
	path := "/_hop/api/services/" + url.PathEscape(name)
	if pid > 0 {
		path += "?pid=" + strconv.Itoa(pid)
	}
	return c.call(http.MethodDelete, path, nil, nil)
}

// apiError keeps the HTTP status, so callers can tell a taken or missing
// name from other failures.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func hasStatus(err error, status int) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.status == status
}

func isNameTaken(err error) bool { return hasStatus(err, http.StatusConflict) }
func isNotFound(err error) bool  { return hasStatus(err, http.StatusNotFound) }

func (c client) call(method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hop is not reachable at %s; is it installed and running? (%w)", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var apiErr struct {
			Error string `json:"error"`
		}
		msg := fmt.Sprintf("%s %s: %s", method, path, resp.Status)
		if json.NewDecoder(resp.Body).Decode(&apiErr) == nil && apiErr.Error != "" {
			msg = apiErr.Error
		}
		return &apiError{status: resp.StatusCode, message: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

// localAppPattern matches the ways people write an app on this machine:
// 3000, :3000, localhost:3000, http://127.0.0.1:3000/ and so on.
var localAppPattern = regexp.MustCompile(`(?i)^(?:([0-9]+)|(?:http://)?(?:localhost|127\.0\.0\.1|\[::1\])?:([0-9]+)/?)$`)

// parseTarget reads what a person types as a destination: a local app's
// port, or a website, with https:// added when no scheme is given.
func parseTarget(raw string) Service {
	raw = strings.TrimSpace(raw)
	if m := localAppPattern.FindStringSubmatch(raw); m != nil {
		port, err := strconv.Atoi(m[1] + m[2]) // exactly one group matched
		if err != nil {
			port = -1 // too large; validation reports it as out of range
		}
		return Service{Port: port}
	}
	// Keep any explicit scheme, so ftp://x is rejected rather than saved as
	// https://ftp://x.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	return Service{URL: raw}
}
