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
		base = "http://" + strings.TrimSuffix(defaultAddr, ":80")
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

// apiError keeps the HTTP status so callers can tell a taken name apart.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func isNameTaken(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusConflict
}

func (c client) remove(name string) error {
	return c.call(http.MethodDelete, "/_hop/api/services/"+url.PathEscape(name), nil, nil)
}

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
		return fmt.Errorf("hop server not reachable at %s (start it with `hop serve`): %w", c.base, err)
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

// parseTarget reads what a person types as a destination: a bare number is a
// local port, anything else is a website, with https:// added when missing.
func parseTarget(raw string) Service {
	raw = strings.TrimSpace(raw)
	if port, err := strconv.Atoi(raw); err == nil {
		return Service{Port: port}
	}
	// Keep any explicit scheme, so ftp://x is rejected rather than saved as
	// https://ftp://x.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	return Service{URL: raw}
}
